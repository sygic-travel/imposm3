package diff

import (
	"fmt"
	"goposm/cache"
	"goposm/config"
	"goposm/database"
	_ "goposm/database/postgis"
	"goposm/diff/parser"
	diffstate "goposm/diff/state"
	"goposm/element"
	"goposm/expire"
	"goposm/geom/geos"
	"goposm/geom/limit"
	"goposm/logging"
	"goposm/mapping"
	"goposm/stats"
	"goposm/writer"
	"io"
)

var log = logging.NewLogger("diff")

func Update(oscFile string, geometryLimiter *limit.Limiter, force bool) {
	state, err := diffstate.ParseFromOsc(oscFile)
	if err != nil {
		log.Fatal(err)
	}
	lastState, err := diffstate.ParseLastState(config.BaseOptions.CacheDir)
	if err != nil {
		log.Fatal(err)
	}

	if lastState != nil && lastState.Sequence != 0 && state != nil && state.Sequence <= lastState.Sequence {
		if !force {
			log.Warn(state, " already imported")
			return
		}
	}

	defer log.StopStep(log.StartStep(fmt.Sprintf("Processing %s", oscFile)))

	elems, errc := parser.Parse(oscFile)

	osmCache := cache.NewOSMCache(config.BaseOptions.CacheDir)
	err = osmCache.Open()
	if err != nil {
		log.Fatal("osm cache: ", err)
	}

	diffCache := cache.NewDiffCache(config.BaseOptions.CacheDir)
	err = diffCache.Open()
	if err != nil {
		log.Fatal("diff cache: ", err)
	}

	tagmapping, err := mapping.NewMapping(config.BaseOptions.MappingFile)
	if err != nil {
		log.Fatal(err)
	}

	connType := database.ConnectionType(config.BaseOptions.Connection)
	dbConf := database.Config{
		Type:             connType,
		ConnectionParams: config.BaseOptions.Connection,
		Srid:             config.BaseOptions.Srid,
	}
	db, err := database.Open(dbConf, tagmapping)
	if err != nil {
		log.Fatal("database open: ", err)
	}

	err = db.Begin()
	if err != nil {
		log.Fatal(err)
	}

	delDb, ok := db.(database.Deleter)
	if !ok {
		log.Fatal("database not deletable")
	}
	deleter := NewDeleter(
		delDb,
		osmCache,
		diffCache,
		tagmapping.PointMatcher(),
		tagmapping.LineStringMatcher(),
		tagmapping.PolygonMatcher(),
	)

	progress := stats.NewStatsReporter()

	expiredTiles := expire.NewTiles(14)

	relTagFilter := tagmapping.RelationTagFilter()
	wayTagFilter := tagmapping.WayTagFilter()
	nodeTagFilter := tagmapping.NodeTagFilter()

	pointsTagMatcher := tagmapping.PointMatcher()
	lineStringsTagMatcher := tagmapping.LineStringMatcher()
	polygonsTagMatcher := tagmapping.PolygonMatcher()

	relations := make(chan *element.Relation)
	ways := make(chan *element.Way)
	nodes := make(chan *element.Node)

	relWriter := writer.NewRelationWriter(osmCache, diffCache, relations,
		db, polygonsTagMatcher, progress, config.BaseOptions.Srid)
	relWriter.SetLimiter(geometryLimiter)
	relWriter.SetExpireTiles(expiredTiles)
	relWriter.Start()

	wayWriter := writer.NewWayWriter(osmCache, diffCache, ways, db,
		lineStringsTagMatcher, polygonsTagMatcher, progress, config.BaseOptions.Srid)
	wayWriter.SetLimiter(geometryLimiter)
	wayWriter.SetExpireTiles(expiredTiles)
	wayWriter.Start()

	nodeWriter := writer.NewNodeWriter(osmCache, nodes, db,
		pointsTagMatcher, progress, config.BaseOptions.Srid)
	nodeWriter.SetLimiter(geometryLimiter)
	nodeWriter.Start()

	nodeIds := make(map[int64]bool)
	wayIds := make(map[int64]bool)
	relIds := make(map[int64]bool)

	step := log.StartStep("Parsing changes, updating cache and removing elements")

	g := geos.NewGeos()
For:
	for {
		select {
		case elem := <-elems:
			if elem.Rel != nil {
				relTagFilter.Filter(&elem.Rel.Tags)
				progress.AddRelations(1)
			} else if elem.Way != nil {
				wayTagFilter.Filter(&elem.Way.Tags)
				progress.AddWays(1)
			} else if elem.Node != nil {
				nodeTagFilter.Filter(&elem.Node.Tags)
				if len(elem.Node.Tags) > 0 {
					progress.AddNodes(1)
				}
				progress.AddCoords(1)
			}
			if elem.Del {
				deleter.Delete(elem)
				if !elem.Add {
					if elem.Rel != nil {
						if err := osmCache.Relations.DeleteRelation(elem.Rel.Id); err != nil {
							log.Fatal(err)
						}
					} else if elem.Way != nil {
						if err := osmCache.Ways.DeleteWay(elem.Way.Id); err != nil {
							log.Fatal(err)
						}
						diffCache.Ways.Delete(elem.Way.Id)
					} else if elem.Node != nil {
						if err := osmCache.Nodes.DeleteNode(elem.Node.Id); err != nil {
							log.Fatal(err)
						}
						if err := osmCache.Coords.DeleteCoord(elem.Node.Id); err != nil {
							log.Fatal(err)
						}
					}
				}
			}
			if elem.Add {
				if elem.Rel != nil {
					// check if first member is cached to avoid caching
					// unneeded relations (typical outside of our coverage)
					if memberIsCached(elem.Rel.Members, osmCache.Ways) {
						osmCache.Relations.PutRelation(elem.Rel)
						relIds[elem.Rel.Id] = true
					}
				} else if elem.Way != nil {
					// check if first coord is cached to avoid caching
					// unneeded ways (typical outside of our coverage)
					if coordIsCached(elem.Way.Refs, osmCache.Coords) {
						osmCache.Ways.PutWay(elem.Way)
						wayIds[elem.Way.Id] = true
					}
				} else if elem.Node != nil {
					if geometryLimiter == nil || geometryLimiter.IntersectsBuffer(g, elem.Node.Long, elem.Node.Lat) {
						osmCache.Nodes.PutNode(elem.Node)
						osmCache.Coords.PutCoords([]element.Node{*elem.Node})
						nodeIds[elem.Node.Id] = true
					}
				}
			}
		case err := <-errc:
			if err != io.EOF {
				log.Fatal(err)
			}
			break For
		}
	}
	progress.Stop()
	log.StopStep(step)
	step = log.StartStep("Writing added/modified elements")

	progress = stats.NewStatsReporter()

	for nodeId, _ := range nodeIds {
		node, err := osmCache.Nodes.GetNode(nodeId)
		if err != nil {
			if err != cache.NotFound {
				log.Print(node, err)
			}
			// missing nodes can still be Coords
			// no `continue` here
		}
		if node != nil {
			// insert new node
			nodes <- node
		}
		dependers := diffCache.Coords.Get(nodeId)
		// mark depending ways for (re)insert
		for _, way := range dependers {
			wayIds[way] = true
		}
	}

	for wayId, _ := range wayIds {
		way, err := osmCache.Ways.GetWay(wayId)
		if err != nil {
			if err != cache.NotFound {
				log.Print(way, err)
			}
			continue
		}
		// insert new way
		ways <- way
		dependers := diffCache.Ways.Get(wayId)
		// mark depending relations for (re)insert
		for _, rel := range dependers {
			relIds[rel] = true
		}
	}

	for relId, _ := range relIds {
		rel, err := osmCache.Relations.GetRelation(relId)
		if err != nil {
			if err != cache.NotFound {
				log.Print(rel, err)
			}
			continue
		}
		// insert new relation
		relations <- rel
	}

	close(relations)
	close(ways)
	close(nodes)

	nodeWriter.Close()
	relWriter.Close()
	wayWriter.Close()

	err = db.End()
	if err != nil {
		log.Fatal(err)
	}
	err = db.Close()
	if err != nil {
		log.Fatal(err)
	}

	osmCache.Close()
	diffCache.Close()
	log.StopStep(step)

	step = log.StartStep("Updating expired tiles db")
	expire.WriteTileExpireDb(
		expiredTiles.SortedTiles(),
		"/tmp/expire_tiles.db",
	)
	log.StopStep(step)
	progress.Stop()

	if state != nil {
		err = diffstate.WriteLastState(config.BaseOptions.CacheDir, state)
		if err != nil {
			log.Warn(err) // warn only
		}
	}
}

func memberIsCached(members []element.Member, wayCache *cache.WaysCache) bool {
	for _, m := range members {
		if m.Type == element.WAY {
			_, err := wayCache.GetWay(m.Id)
			if err != nil {
				return false
			}
			return true
		}
	}
	return false
}

func coordIsCached(refs []int64, coordCache *cache.DeltaCoordsCache) bool {
	if len(refs) <= 0 {
		return false
	}
	_, err := coordCache.GetCoord(refs[0])
	if err != nil {
		return false
	}
	return true
}

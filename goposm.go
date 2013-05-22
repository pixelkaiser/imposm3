package main

import (
	"flag"
	"goposm/cache"
	"goposm/database"
	_ "goposm/database/postgis"
	"goposm/mapping"
	"goposm/reader"
	"goposm/stats"
	"goposm/writer"
	"log"
	"os"
	"runtime"
	"runtime/pprof"
	"strconv"
	"strings"
	"time"
)

var dbImportBatchSize int64

func init() {
	dbImportBatchSize, _ = strconv.ParseInt(
		os.Getenv("GOPOSM_DBIMPORT_BATCHSIZE"), 10, 32)

	if dbImportBatchSize == 0 {
		dbImportBatchSize = 4096
	}
}

var (
	cpuprofile       = flag.String("cpuprofile", "", "filename of cpu profile output")
	memprofile       = flag.String("memprofile", "", "dir name of mem profile output and interval (fname:interval)")
	cachedir         = flag.String("cachedir", "/tmp/goposm", "cache directory")
	overwritecache   = flag.Bool("overwritecache", false, "overwritecache")
	appendcache      = flag.Bool("appendcache", false, "append cache")
	read             = flag.String("read", "", "read")
	write            = flag.Bool("write", false, "write")
	connection       = flag.String("connection", "", "connection parameters")
	diff             = flag.Bool("diff", false, "enable diff support")
	mappingFile      = flag.String("mapping", "", "mapping file")
	deployProduction = flag.Bool("deployproduction", false, "deploy production")
	revertDeploy     = flag.Bool("revertdeploy", false, "revert deploy to production")
	removeBackup     = flag.Bool("removebackup", false, "remove backups from deploy")
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	runtime.GOMAXPROCS(runtime.NumCPU())
	flag.Parse()

	if *cpuprofile != "" {
		f, err := os.Create(*cpuprofile)
		if err != nil {
			log.Fatal(err)
		}
		pprof.StartCPUProfile(f)
		defer pprof.StopCPUProfile()
	}

	if *memprofile != "" {
		parts := strings.Split(*memprofile, string(os.PathListSeparator))
		var interval time.Duration

		if len(parts) < 2 {
			interval, _ = time.ParseDuration("1m")
		} else {
			var err error
			interval, err = time.ParseDuration(parts[1])
			if err != nil {
				log.Fatal(err)
			}
		}

		go stats.MemProfiler(parts[0], interval)
	}

	if (*write || *read != "") && (*revertDeploy || *removeBackup) {
		log.Fatal("-revertdeploy and -removebackup not compatible with -read/-write")
	}

	if *revertDeploy && (*removeBackup || *deployProduction) {
		log.Fatal("-revertdeploy not compatible with -deployproduction/-removebackup")
	}

	osmCache := cache.NewOSMCache(*cachedir)

	if *read != "" && osmCache.Exists() {
		if *overwritecache {
			log.Println("removing existing cache", *cachedir)
			err := osmCache.Remove()
			if err != nil {
				log.Fatal("unable to remove cache:", err)
			}
		} else if !*appendcache {
			log.Fatal("cache already exists use -appendcache or -overwritecache")
		}
	}

	err := osmCache.Open()
	if err != nil {
		log.Fatal(err)
	}
	defer osmCache.Close()

	progress := stats.StatsReporter()

	tagmapping, err := mapping.NewMapping(*mappingFile)
	if err != nil {
		log.Fatal(err)
	}

	var db database.DB

	if *write || *deployProduction || *revertDeploy || *removeBackup {
		connType := database.ConnectionType(*connection)
		conf := database.Config{
			Type:             connType,
			ConnectionParams: *connection,
			Srid:             3857,
		}
		db, err = database.Open(conf, tagmapping)
		if err != nil {
			log.Fatal(err)
		}
	}

	if *read != "" {
		osmCache.Coords.SetLinearImport(true)
		reader.ReadPbf(osmCache, progress, tagmapping, *read)
		osmCache.Coords.SetLinearImport(false)
		progress.Reset()
		osmCache.Coords.Flush()
	}

	if *write {
		progress.Reset()
		err = db.Init()
		if err != nil {
			log.Fatal(err)
		}

		diffCache := cache.NewDiffCache(*cachedir)
		if err = diffCache.Remove(); err != nil {
			log.Fatal(err)
		}
		if err = diffCache.Open(); err != nil {
			log.Fatal(err)
		}

		insertBuffer := writer.NewInsertBuffer()
		dbWriter := writer.NewDbWriter(db, insertBuffer.Out)

		pointsTagMatcher := tagmapping.PointMatcher()
		lineStringsTagMatcher := tagmapping.LineStringMatcher()
		polygonsTagMatcher := tagmapping.PolygonMatcher()

		relations := osmCache.Relations.Iter()
		relWriter := writer.NewRelationWriter(osmCache, relations,
			insertBuffer, polygonsTagMatcher, progress)
		// blocks till the Relations.Iter() finishes
		relWriter.Close()

		ways := osmCache.Ways.Iter()
		wayWriter := writer.NewWayWriter(osmCache, ways, insertBuffer,
			lineStringsTagMatcher, polygonsTagMatcher, progress)

		nodes := osmCache.Nodes.Iter()
		nodeWriter := writer.NewNodeWriter(osmCache, nodes, insertBuffer,
			pointsTagMatcher, progress)

		diffCache.Coords.Close()

		wayWriter.Close()
		nodeWriter.Close()
		insertBuffer.Close()
		dbWriter.Close()

		if db, ok := db.(database.Finisher); ok {
			if err := db.Finish(); err != nil {
				log.Fatal(err)
			}
		} else {
			log.Fatal("database not finishable")
		}
	}

	if *deployProduction {
		if db, ok := db.(database.Deployer); ok {
			if err := db.Deploy(); err != nil {
				log.Fatal(err)
			}
		} else {
			log.Fatal("database not deployable")
		}
	}

	if *revertDeploy {
		if db, ok := db.(database.Deployer); ok {
			if err := db.RevertDeploy(); err != nil {
				log.Fatal(err)
			}
		} else {
			log.Fatal("database not deployable")
		}
	}

	if *removeBackup {
		if db, ok := db.(database.Deployer); ok {
			if err := db.RemoveBackup(); err != nil {
				log.Fatal(err)
			}
		} else {
			log.Fatal("database not deployable")
		}
	}
	progress.Stop()

}

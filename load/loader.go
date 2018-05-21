package load

import (
	"bufio"
	"flag"
	"fmt"
	"math"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// DefaultBatchSize is the default size of batches to be inserted
	defaultBatchSize = 10000
	defaultReadSize  = 4 << 20 // 4 MB

	// WorkerPerQueue is the value to have each worker have its own queue of batches
	WorkerPerQueue = 0
	// SingleQueue is the value to have only a single shared queue of work for all workers
	SingleQueue = 1
)

// Benchmark is an interface that represents the skeleton of a program
// needed to run an insert or load benchmark.
type Benchmark interface {
	// GetPointDecoder returns the PointDecoder to use for this Benchmark
	GetPointDecoder(br *bufio.Reader) PointDecoder
	// GetBatchFactory returns the BatchFactory to use for this Benchmark
	GetBatchFactory() BatchFactory
	// GetPointIndexer returns the PointIndexer to use for this Benchmark
	GetPointIndexer(maxPartitions uint) PointIndexer
	// GetProcessor returns the Processor to use for this Benchmark
	GetProcessor() Processor
}

// BenchmarkRunner is responsible for initializing and storing common
// flags across all database systems and ultimately running a supplied Benchmark
type BenchmarkRunner struct {
	dbName          string
	batchSize       int
	workers         uint
	limit           int64
	doLoad          bool
	doInit          bool
	reportingPeriod time.Duration
	filename        string // TODO implement file reading

	// non-flag fields
	br *bufio.Reader
}

var loader = &BenchmarkRunner{}

// GetBenchmarkRunner returns the singleton BenchmarkRunner for use in a benchmark program
// with a batch size of 10000
func GetBenchmarkRunner() *BenchmarkRunner {
	return GetBenchmarkRunnerWithBatchSize(defaultBatchSize)
}

// GetBenchmarkRunnerWithBatchSize returns the singleton BenchmarkRunner for use in a benchmark program
// with a non-default batch size.
func GetBenchmarkRunnerWithBatchSize(batchSize int) *BenchmarkRunner {
	flag.StringVar(&loader.dbName, "db-name", "benchmark", "Name of database")

	flag.IntVar(&loader.batchSize, "batch-size", batchSize, "Number of items to batch together in a single insert")
	flag.UintVar(&loader.workers, "workers", 1, "Number of parallel clients inserting")
	flag.Int64Var(&loader.limit, "limit", -1, "Number of items to insert (default unlimited).")
	flag.BoolVar(&loader.doLoad, "do-load", true, "Whether to write data. Set this flag to false to check input read speed")
	flag.BoolVar(&loader.doInit, "do-init", true, "Whether to initialize the database. Disable on all but one box if running on a multi client box setup.")
	flag.DurationVar(&loader.reportingPeriod, "reporting-period", 10*time.Second, "Period to report write stats")

	return loader
}

// DatabaseName returns the value of the --db-name flag (name of the database to store data)
func (l *BenchmarkRunner) DatabaseName() string {
	return l.dbName
}

// DoLoad returns the value of the --do-load flag (whether to actually load or not)
func (l *BenchmarkRunner) DoLoad() bool {
	return l.doLoad
}

// DoInit returns the value of the --do-init flag (whether to actually initialize the DB or not)
func (l *BenchmarkRunner) DoInit() bool {
	return l.doInit
}

// RunBenchmark takes in a Benchmark b, a bufio.Reader br, and holders for number of metrics and rows
// and uses those to run the load benchmark
func (l *BenchmarkRunner) RunBenchmark(b Benchmark, workQueues uint, metricCount, rowCount *uint64) {
	l.br = l.GetBufferedReader()
	var wg sync.WaitGroup

	channels := []*duplexChannel{}
	maxPartitions := workQueues
	if workQueues == WorkerPerQueue {
		maxPartitions = l.workers
	} else if workQueues > l.workers {
		panic(fmt.Sprintf("cannot have more work queues (%d) than workers (%d)", workQueues, l.workers))
	}
	perQueue := int(math.Ceil(float64(l.workers / maxPartitions)))
	for i := uint(0); i < maxPartitions; i++ {
		channels = append(channels, newDuplexChannel(perQueue))
	}

	for i := 0; i < int(l.workers); i++ {
		wg.Add(1)
		go work(b, &wg, channels[i%len(channels)], i, l.doLoad)
	}

	start := time.Now()
	l.scan(b, channels, maxPartitions, metricCount, rowCount)

	for _, c := range channels {
		c.close()
	}
	wg.Wait()
	end := time.Now()

	summary(end.Sub(start), l.workers, metricCount, rowCount)
}

// GetBufferedReader returns the buffered Reader that should be used by the loader
func (l *BenchmarkRunner) GetBufferedReader() *bufio.Reader {
	if l.br == nil {
		if len(l.filename) > 0 {
			l.br = nil // TODO - Support reading from files
		} else {
			l.br = bufio.NewReaderSize(os.Stdin, defaultReadSize)
		}
	}
	return l.br
}

// scan launches any needed reporting mechanism and proceeds to scan input data
// to distribute to workers
func (l *BenchmarkRunner) scan(b Benchmark, channels []*duplexChannel, maxPartitions uint, metricCount, rowCount *uint64) int64 {
	if l.reportingPeriod.Nanoseconds() > 0 {
		go report(l.reportingPeriod, metricCount, rowCount)
	}
	return scanWithIndexer(channels, l.batchSize, l.limit, l.br, b.GetPointDecoder(l.br), b.GetBatchFactory(), b.GetPointIndexer(maxPartitions))
}

// work is the processing function for each worker in the loader
func work(b Benchmark, wg *sync.WaitGroup, c *duplexChannel, workerNum int, doLoad bool) {
	proc := b.GetProcessor()
	proc.Init(workerNum, doLoad)
	for b := range c.toWorker {
		proc.ProcessBatch(b, doLoad)
		c.sendToScanner()
	}
	wg.Done()
	switch c := proc.(type) {
	case ProcessorCloser:
		c.Close(doLoad)
	}
}

// summary prints the summary of statistics from loading
func summary(took time.Duration, workers uint, metricCount, rowCount *uint64) {
	metricRate := float64(*metricCount) / float64(took.Seconds())
	fmt.Println("\nSummary:")
	fmt.Printf("loaded %d metrics in %0.3fsec with %d workers (mean rate %0.2f metrics/sec)\n", *metricCount, took.Seconds(), workers, metricRate)
	if rowCount != nil {
		rowRate := float64(*rowCount) / float64(took.Seconds())
		fmt.Printf("loaded %d rows in %0.3fsec with %d workers (mean rate %0.2f rows/sec)\n", *rowCount, took.Seconds(), workers, rowRate)
	}
}

// report handles periodic reporting of loading stats
func report(period time.Duration, metricCount, rowCount *uint64) {
	start := time.Now()
	prevTime := start
	prevColCount := uint64(0)
	prevRowCount := uint64(0)

	rCount := uint64(0)
	fmt.Printf("time,per. metric/s,metric total,overall metric/s,per. row/s,row total,overall row/s\n")
	for now := range time.NewTicker(period).C {
		cCount := atomic.LoadUint64(metricCount)
		if rowCount != nil {
			rCount = atomic.LoadUint64(rowCount)
		}

		sinceStart := now.Sub(start)
		took := now.Sub(prevTime)
		colrate := float64(cCount-prevColCount) / float64(took.Seconds())
		overallColRate := float64(cCount) / float64(sinceStart.Seconds())
		if rowCount != nil {
			rowrate := float64(rCount-prevRowCount) / float64(took.Seconds())
			overallRowRate := float64(rCount) / float64(sinceStart.Seconds())
			fmt.Printf("%d,%0.3f,%E,%0.3f,%0.3f,%E,%0.3f\n", now.Unix(), colrate, float64(cCount), overallColRate, rowrate, float64(rCount), overallRowRate)
		} else {
			fmt.Printf("%d,%0.3f,%E,%0.3f,-,-,-\n", now.Unix(), colrate, float64(cCount), overallColRate)
		}

		prevColCount = cCount
		prevRowCount = rCount
		prevTime = now
	}
}

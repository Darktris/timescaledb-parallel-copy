// timescaledb-parallel-copy loads data from CSV format into a TimescaleDB database
package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"
)

const (
	binName = "timescaledb-parallel-copy"
	version = "0.1.1-beta"
)

// Flag vars
var (
	postgresConnect string
	dbName          string
	schemaName      string
	tableName       string
	truncate        bool

	copyOptions    string
	splitCharacter string
	joinCharacter  string
	fromFile       string
	columns        string

	workers         int
	batchSize       int
	logBatches      bool
	reportingPeriod time.Duration
	verbose         bool
	showVersion     bool

	columnCount int64
	rowCount    int64
	tsColumns   []uint64
	stringOfTs  string
)

type batch struct {
	rows []string
}

// Parse args
func init() {
	flag.StringVar(&postgresConnect, "connection", "host=localhost user=postgres sslmode=disable", "PostgreSQL connection url")
	flag.StringVar(&dbName, "db-name", "test", "Database where the destination table exists")
	flag.StringVar(&tableName, "table", "test_table", "Destination table for insertions")
	flag.StringVar(&schemaName, "schema", "public", "Desination table's schema")
	flag.BoolVar(&truncate, "truncate", false, "Truncate the destination table before insert")

	flag.StringVar(&copyOptions, "copy-options", "CSV", "Additional options to pass to COPY (e.g., NULL 'NULL')")
	flag.StringVar(&splitCharacter, "split", ",", "Character to split by")
	flag.StringVar(&joinCharacter, "split", "|", "Character to join by")
	flag.StringVar(&fromFile, "file", "", "File to read from rather than stdin")
	flag.StringVar(&columns, "columns", "", "Comma-separated columns present in CSV")

	flag.IntVar(&batchSize, "batch-size", 5000, "Number of rows per insert")
	flag.IntVar(&workers, "workers", 1, "Number of parallel requests to make")
	flag.BoolVar(&logBatches, "log-batches", false, "Whether to time individual batches.")
	flag.DurationVar(&reportingPeriod, "reporting-period", 0*time.Second, "Period to report insert stats; if 0s, intermediate results will not be reported")
	flag.BoolVar(&verbose, "verbose", false, "Print more information about copying statistics")

	flag.BoolVar(&showVersion, "version", false, "Show the version of this tool")

	flag.StringVar(&stringOfTs, "convert-from-epoch", "", "Columns that need to be converted from epoch to Date. Comma separated, e.g. -convert-from-epoch 1,2,3 .")
	flag.Parse()

	tsStrings := strings.Split(stringOfTs, ",") // Columns that need TS conversion
	if len(tsStrings) == 1 {
		if len(tsStrings[0]) == 0 {
			tsStrings = make([]string, 0)
		}
	}
	tsColumns = make([]uint64, len(tsStrings))
	for i, tsString := range tsStrings {
		tsColumns[i], _ = strconv.ParseUint(tsString, 10, 64)
	}
	//fmt.Println("tsColumns", tsColumns)
}

func getConnectString() string {
	return fmt.Sprintf("%s dbname=%s", postgresConnect, dbName)
}

func getFullTableName() string {
	return fmt.Sprintf("\"%s\".\"%s\"", schemaName, tableName)
}

func main() {
	if showVersion {
		fmt.Printf("%s %s (%s %s)\n", binName, version, runtime.GOOS, runtime.GOARCH)
		os.Exit(0)
	}

	if truncate { // Remove existing data from the table
		dbBench := sqlx.MustConnect("postgres", getConnectString())
		_, err := dbBench.Exec(fmt.Sprintf("TRUNCATE %s", getFullTableName()))
		if err != nil {
			panic(err)
		}

		err = dbBench.Close()
		if err != nil {
			panic(err)
		}
	}

	var scanner *bufio.Scanner
	if len(fromFile) > 0 {
		file, err := os.Open(fromFile)
		if err != nil {
			log.Fatal(err)
		}
		defer file.Close()

		scanner = bufio.NewScanner(file)
	} else {
		scanner = bufio.NewScanner(os.Stdin)
	}

	var wg sync.WaitGroup
	batchChan := make(chan *batch, workers)

	// Generate COPY workers
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go processBatches(&wg, batchChan)
	}

	// Reporting thread
	if reportingPeriod > (0 * time.Second) {
		go report()
	}

	start := time.Now()
	rowsRead := scan(batchSize, scanner, batchChan)
	close(batchChan)
	wg.Wait()
	end := time.Now()
	took := end.Sub(start)
	rowRate := float64(rowsRead) / float64(took.Seconds())

	res := fmt.Sprintf("COPY %d", rowsRead)
	if verbose {
		res += fmt.Sprintf(", took %v with %d worker(s) (mean rate %f/sec)", took, workers, rowRate)
	}
	fmt.Println(res)
}

// report periodically prints the write rate in number of rows per second
func report() {
	start := time.Now()
	prevTime := start
	prevRowCount := int64(0)

	for now := range time.NewTicker(reportingPeriod).C {
		rCount := atomic.LoadInt64(&rowCount)

		took := now.Sub(prevTime)
		rowrate := float64(rCount-prevRowCount) / float64(took.Seconds())
		overallRowrate := float64(rCount) / float64(now.Sub(start).Seconds())
		totalTook := now.Sub(start)

		fmt.Printf("at %v, row rate %f/sec (period), row rate %f/sec (overall), %E total rows\n", totalTook-(totalTook%time.Second), rowrate, overallRowrate, float64(rCount))

		prevRowCount = rCount
		prevTime = now
	}

}

// scan reads lines from a bufio.Scanner, each which should be in CSV format
// with a delimiter specified by --split (comma by default)
func scan(itemsPerBatch int, scanner *bufio.Scanner, batchChan chan *batch) int64 {
	rows := make([]string, 0, itemsPerBatch)
	var linesRead int64
	var outputline string
	for scanner.Scan() {
		linesRead++

		outputline = scanner.Text()
		rows = append(rows, outputline)
		if len(rows) >= itemsPerBatch { // dispatch to COPY worker & reset
			batchChan <- &batch{rows}
			rows = make([]string, 0, itemsPerBatch)
		}
	}

	if err := scanner.Err(); err != nil {
		log.Fatalf("Error reading input: %s", err.Error())
	}

	// Finished reading input, make sure last batch goes out.
	if len(rows) > 0 {
		batchChan <- &batch{rows}
	}

	return linesRead
}

// processBatches reads batches from C and writes them to the target server, while tracking stats on the write.
func processBatches(wg *sync.WaitGroup, C chan *batch) {
	var tsFloat float64
	dbBench := sqlx.MustConnect("postgres", getConnectString())
	defer dbBench.Close()
	splitter := regexp.MustCompile(splitCharacter)
	var sp []string

	columnCountWorker := int64(0)
	for batch := range C {
		start := time.Now()

		tx := dbBench.MustBegin()
		delimStr := fmt.Sprintf("'%s'", joinCharacter)
		if joinCharacter == "\\t" {
			delimStr = "E" + delimStr
		}
		var copyCmd string
		if columns != "" {
			copyCmd = fmt.Sprintf("COPY %s(%s) FROM STDIN WITH DELIMITER %s %s", getFullTableName(), columns, delimStr, copyOptions)
		} else {
			copyCmd = fmt.Sprintf("COPY %s FROM STDIN WITH DELIMITER %s %s", getFullTableName(), delimStr, copyOptions)
		}

		stmt, err := tx.Prepare(copyCmd)
		if err != nil {
			panic(err)
		}

		// Need to cover the string-ified version of the character to actual character for correct split
		sChar := splitCharacter
		if sChar == "\\t" {
			sChar = "\t"
		}
		for _, line := range batch.rows {
			sp = splitter.Split(line, -1)

			for _, tsIndex := range tsColumns {
				tsFloat, _ = strconv.ParseFloat(sp[tsIndex], 64)
				sp[tsIndex] = time.Unix(0, int64(tsFloat*1e9)).UTC().Format(time.UnixDate)
			}

			line = strings.Join(sp, joinCharacter)
			columnCountWorker += int64(len(sp))
			// For some reason this is only needed for tab splitting
			if sChar == "\t" {
				args := make([]interface{}, len(sp))
				for i, v := range sp {
					args[i] = v
				}
				_, err = stmt.Exec(args...)
			} else {

				_, err = stmt.Exec(line)
			}

			if err != nil {
				panic(err)
			}
		}
		atomic.AddInt64(&columnCount, columnCountWorker)
		atomic.AddInt64(&rowCount, int64(len(batch.rows)))
		columnCountWorker = 0

		err = stmt.Close()
		if err != nil {
			panic(err)
		}

		err = tx.Commit()
		if err != nil {
			panic(err)
		}

		if logBatches {
			took := time.Now().Sub(start)
			fmt.Printf("[BATCH] took %v, batch size %d, row rate %f/sec\n", took, batchSize, float64(batchSize)/float64(took.Seconds()))
		}

	}
	wg.Done()
}

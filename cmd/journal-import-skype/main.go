// Command journal-import-skype imports a NirSoft SkypeLogView text export into
// journal-store as transcripts: one private entry of kind transcript per
// conversation per day, every line carrying the log's Record Number, so a
// timeline item can cite the day and the reader can read the conversation
// whole.
//
//	journal-import-skype -log skype-log.txt.gz -time-zone America/Chicago -self me_x \
//	    -people skype-people.json -source-entry entry_000001 -dry-run
//
// -people is a JSON object from Skype name to people-store id; those people
// are drawn as chips in each transcript's header. The source is skype and
// source_ref is "<conversation>/<day>", so running it again skips every day
// whose text did not change.
package main

import (
	"compress/gzip"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	journalstore "github.com/kayushkin/journal-store"
	"github.com/kayushkin/journal-store/internal/importclient"
	"github.com/kayushkin/journal-store/internal/skypelog"
)

func main() {
	logPath := flag.String("log", "", "the SkypeLogView export, UTF-8 text, optionally gzipped (.gz)")
	timeZone := flag.String("time-zone", "", "IANA zone the export's clock was in (America/Chicago); required, since the export names none")
	self := flag.String("self", "", "the user's own Skype name; a one-to-one chat is titled by the other side")
	peoplePath := flag.String("people", "", "JSON object mapping Skype names to people-store ids, drawn as chips")
	sourceEntry := flag.String("source-entry", "", "the journal-store entry that describes the log, linked from every transcript")
	source := flag.String("source", "skype", "the import source every transcript is filed under")
	earliestYear := flag.Int("earliest-year", 2000, "drop records dated before this year (calls with a bad clock)")
	storeURL := flag.String("store-url", "http://"+journalstore.DefaultListenAddress, "journal-store's base URL")
	dryRun := flag.Bool("dry-run", false, "ask the store what it would do and write nothing")
	batchSize := flag.Int("batch", 100, "transcripts per POST /imports request")
	flag.Parse()
	if *logPath == "" || *timeZone == "" || *self == "" || *batchSize < 1 {
		flag.Usage()
		os.Exit(2)
	}
	if err := run(*logPath, *timeZone, *self, *peoplePath, *sourceEntry, *source, *earliestYear, *storeURL, *dryRun, *batchSize, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "journal-import-skype: %v\n", err)
		os.Exit(1)
	}
}

func run(logPath, timeZone, self, peoplePath, sourceEntry, source string, earliestYear int, storeURL string, dryRun bool, batchSize int, out io.Writer) error {
	location, err := time.LoadLocation(timeZone)
	if err != nil {
		return fmt.Errorf("-time-zone: %w", err)
	}
	people := map[string]string{}
	if peoplePath != "" {
		raw, err := os.ReadFile(peoplePath)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(raw, &people); err != nil {
			return fmt.Errorf("-people %s: %w", peoplePath, err)
		}
	}
	file, err := os.Open(logPath)
	if err != nil {
		return err
	}
	defer file.Close()
	var reader io.Reader = file
	if strings.HasSuffix(logPath, ".gz") {
		unzipped, err := gzip.NewReader(file)
		if err != nil {
			return err
		}
		defer unzipped.Close()
		reader = unzipped
	}
	records, err := skypelog.Parse(reader, location)
	if err != nil {
		return err
	}
	transcripts, skipped := skypelog.Transcripts(records, location, skypelog.Options{
		Self: self, People: people, SourceEntry: sourceEntry, EarliestYear: earliestYear,
	})
	fmt.Fprintf(out, "read %d records into %d transcripts; left out %d with no time, %d with a bad clock, %d empty\n",
		len(records), len(transcripts), skipped.NoTime, skipped.BadClock, skipped.EmptyChats)
	entries := make([]journalstore.ImportEntry, 0, len(transcripts))
	for _, transcript := range transcripts {
		entries = append(entries, journalstore.ImportEntry{
			Source: source, SourceRef: transcript.ConversationKey + "/" + transcript.Day, SourceHash: transcript.Hash,
			Kind: journalstore.KindTranscript, Visibility: journalstore.VisibilityPrivate,
			Title: transcript.Title, Body: transcript.Body, WrittenAt: transcript.FirstTime.Unix(),
			Tags: []string{source},
		})
	}
	total, err := importclient.PostAll(storeURL, entries, batchSize, dryRun, func(journalstore.ImportOutcome) {})
	if err != nil {
		return err
	}
	verb := "imported"
	if dryRun {
		verb = "dry run, nothing written; would have"
	}
	fmt.Fprintf(out, "%s: created %d, updated %d, unchanged %d\n", verb, total.Created, total.Updated, total.Skipped)
	return nil
}

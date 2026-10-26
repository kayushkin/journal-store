// Command journal-import-facebook-messages imports the Messenger chats of a
// Facebook data export (JSON format) into journal-store as transcripts: one
// private entry of kind transcript per thread per day, both sides. Threads
// of more than eight people are left out.
//
//	journal-import-facebook-messages -messages <export>/your_facebook_activity/messages \
//	    -self-name 'Me Self' -time-zones 'Europe/Lisbon,2021-07-11=America/Chicago' \
//	    -people people.json -source-entry entry_000001 -dry-run
//
// -people is a JSON object from a name as the export writes it to a
// people-store id, drawn as a chip; the export names people only by name.
// The source is facebook-messenger and source_ref is "<folder>/<thread>/<day>",
// so running it again skips every day whose text did not change.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	journalstore "github.com/kayushkin/journal-store"
	"github.com/kayushkin/journal-store/internal/facebookmessages"
	"github.com/kayushkin/journal-store/internal/importclient"
	"github.com/kayushkin/journal-store/internal/residencezones"
)

func main() {
	messagesDir := flag.String("messages", "", "the export's your_facebook_activity/messages folder")
	selfName := flag.String("self-name", "", "the user's own name as the export writes it")
	timeZones := flag.String("time-zones", "", "where the user lived: first zone, then day=zone for each move (Europe/Lisbon,2021-07-11=America/Chicago)")
	peoplePath := flag.String("people", "", "JSON object mapping names as the export writes them to people-store ids, drawn as chips")
	sourceEntry := flag.String("source-entry", "", "the journal-store entry that describes the export, linked from every transcript")
	source := flag.String("source", "facebook-messenger", "the import source every transcript is filed under")
	storeURL := flag.String("store-url", "http://"+journalstore.DefaultListenAddress, "journal-store's base URL")
	dryRun := flag.Bool("dry-run", false, "ask the store what it would do and write nothing")
	batchSize := flag.Int("batch", 100, "transcripts per POST /imports request")
	flag.Parse()
	if *messagesDir == "" || *selfName == "" || *timeZones == "" || *batchSize < 1 {
		flag.Usage()
		os.Exit(2)
	}
	if err := run(*messagesDir, *selfName, *timeZones, *peoplePath, *sourceEntry, *source, *storeURL, *dryRun, *batchSize, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "journal-import-facebook-messages: %v\n", err)
		os.Exit(1)
	}
}

func run(messagesDir, selfName, timeZones, peoplePath, sourceEntry, source, storeURL string, dryRun bool, batchSize int, out io.Writer) error {
	zones, err := residencezones.Parse(timeZones)
	if err != nil {
		return fmt.Errorf("-time-zones: %w", err)
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
	threads, skipped, err := facebookmessages.Read(messagesDir)
	if err != nil {
		return err
	}
	transcripts := facebookmessages.Transcripts(threads, facebookmessages.Options{
		SelfName: selfName, People: people, SourceEntry: sourceEntry, Zones: zones,
	})
	fmt.Fprintf(out, "read %d threads into %d transcripts; left out %d threads of more than %d people (%d messages)\n",
		len(threads), len(transcripts), skipped.TooManyParticipants, facebookmessages.MaximumParticipants, skipped.Messages)
	entries := make([]journalstore.ImportEntry, 0, len(transcripts))
	for _, transcript := range transcripts {
		entries = append(entries, journalstore.ImportEntry{
			Source: source, SourceRef: transcript.Folder + "/" + transcript.ThreadID + "/" + transcript.Day, SourceHash: transcript.Hash,
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

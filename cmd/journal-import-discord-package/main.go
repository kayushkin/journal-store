// Command journal-import-discord-package imports the direct messages and
// group direct messages of a Discord data package into journal-store as
// transcripts: one private entry of kind transcript per channel per day. The
// package holds only the owner's own messages, and every transcript says so.
// Server channels are left out.
//
//	journal-import-discord-package -messages <package>/Messages -self-id 100000000000000001 -self-name Me \
//	    -time-zones 'Europe/Lisbon,2021-07-11=America/Chicago' \
//	    -people people.json -names names.json -source-entry entry_000001 -dry-run
//
// -people is a JSON object from a Discord user id or username to a
// people-store id, drawn as a chip; -full-history is a folder of whole
// conversations fetched from Discord, which give those channels both sides; -names is a JSON object from a Discord
// user id to a name to show. The source is discord-package and source_ref is
// "<channel id>/<day>", so running it again skips every day whose text did
// not change.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	journalstore "github.com/kayushkin/journal-store"
	"github.com/kayushkin/journal-store/internal/discordpackage"
	"github.com/kayushkin/journal-store/internal/importclient"
	"github.com/kayushkin/journal-store/internal/residencezones"
)

func main() {
	messagesDir := flag.String("messages", "", "the package's Messages folder, holding index.json and c<channel id>/ folders")
	selfID := flag.String("self-id", "", "the owner's Discord user id")
	selfName := flag.String("self-name", "", "the name the owner's lines are signed with")
	timeZones := flag.String("time-zones", "", "where the owner lived: first zone, then day=zone for each move (Europe/Lisbon,2021-07-11=America/Chicago)")
	peoplePath := flag.String("people", "", "JSON object mapping Discord user ids or usernames to people-store ids, drawn as chips")
	namesPath := flag.String("names", "", "JSON object mapping Discord user ids to names to show")
	fullHistory := flag.String("full-history", "", "a folder of whole-conversation fetches (multichat-fetch-discord-history -direct-message): channels found there get both sides")
	sourceEntry := flag.String("source-entry", "", "the journal-store entry that describes the package, linked from every transcript")
	source := flag.String("source", "discord-package", "the import source every transcript is filed under")
	storeURL := flag.String("store-url", "http://"+journalstore.DefaultListenAddress, "journal-store's base URL")
	dryRun := flag.Bool("dry-run", false, "ask the store what it would do and write nothing")
	batchSize := flag.Int("batch", 100, "transcripts per POST /imports request")
	flag.Parse()
	if *messagesDir == "" || *selfID == "" || *selfName == "" || *timeZones == "" || *batchSize < 1 {
		flag.Usage()
		os.Exit(2)
	}
	if err := run(*messagesDir, *selfID, *selfName, *timeZones, *peoplePath, *namesPath, *fullHistory, *sourceEntry, *source, *storeURL, *dryRun, *batchSize, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "journal-import-discord-package: %v\n", err)
		os.Exit(1)
	}
}

func readObject(path string) (map[string]string, error) {
	object := map[string]string{}
	if path == "" {
		return object, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return object, nil
}

func run(messagesDir, selfID, selfName, timeZones, peoplePath, namesPath, fullHistory, sourceEntry, source, storeURL string, dryRun bool, batchSize int, out io.Writer) error {
	zones, err := residencezones.Parse(timeZones)
	if err != nil {
		return fmt.Errorf("-time-zones: %w", err)
	}
	people, err := readObject(peoplePath)
	if err != nil {
		return fmt.Errorf("-people: %w", err)
	}
	names, err := readObject(namesPath)
	if err != nil {
		return fmt.Errorf("-names: %w", err)
	}
	channels, leftOut, err := discordpackage.Read(messagesDir)
	if err != nil {
		return err
	}
	withFullHistory := 0
	if fullHistory != "" {
		histories, fetchedAt, err := discordpackage.ReadFullHistory(fullHistory)
		if err != nil {
			return fmt.Errorf("-full-history: %w", err)
		}
		withFullHistory = discordpackage.MergeFullHistory(channels, histories, fetchedAt)
	}
	transcripts := discordpackage.Transcripts(channels, discordpackage.Options{
		SelfID: selfID, SelfName: selfName, People: people, Names: names, SourceEntry: sourceEntry, Zones: zones,
	})
	fmt.Fprintf(out, "read %d DM and group DM channels (%d with both sides fetched) into %d transcripts; left out %d server channels\n", len(channels), withFullHistory, len(transcripts), leftOut)
	entries := make([]journalstore.ImportEntry, 0, len(transcripts))
	for _, transcript := range transcripts {
		entries = append(entries, journalstore.ImportEntry{
			Source: source, SourceRef: transcript.ChannelID + "/" + transcript.Day, SourceHash: transcript.Hash,
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

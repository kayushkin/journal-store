// Command journal-import-android-messages imports the text and picture
// messages copied off one or more Android phones into journal-store as
// transcripts: one private entry of kind transcript per conversation per day.
//
//	journal-import-android-messages \
//	    -phone 'old|Old Phone|<dir>' -phone 'new|New Phone|<dir>' \
//	    -self-name 'Me' -time-zones 'Europe/Lisbon,2021-07-11=America/Chicago' \
//	    -source-entry <entry id> -photo-source-machine <host> -dry-run
//
// Each -phone is "tag|name|folder": the folder holds the phone's content
// query exports (see internal/androidmessages). Phones often carry the same
// history from one to the next, so every message is kept once, however many
// phones held it.
//
// Only direct conversations and groups of at most eight people the user
// wrote in are imported. Each number is named through people-store
// (GET /identities/resolve) and drawn as a chip; a number nobody has
// becomes a people-store suggestion (source phone_messages), never a link.
// A message that is a login code or a password is written as "a login code"
// or "a password", without its text.
//
// Pictures and videos go to photo-store as thumbnails, cited by the
// transcript (source_path is the entry id, date_source message), and the
// transcript cites the photo by id. A photo tagged private-document, deleted
// or purged in photo-store is never linked. A picture photo-store already
// holds from elsewhere is cited, not imported again.
//
// Calls join the transcript of a day the conversation has texts on; a call
// on any other day is left out and counted.
//
// The source is android-messages and source_ref "<numbers>/<day>", so
// running it again skips every unchanged day. A day an earlier import already
// holds under -fold-into-source with the same source_ref is written there
// instead, so it is extended rather than doubled.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	journalstore "github.com/kayushkin/journal-store"
	"github.com/kayushkin/journal-store/internal/androidmessages"
	"github.com/kayushkin/journal-store/internal/importclient"
	"github.com/kayushkin/journal-store/internal/residencezones"
	peoplestore "github.com/kayushkin/people-store"
	photostore "github.com/kayushkin/photo-store"
	"github.com/kayushkin/photo-store/thumbnail"
)

// privateDocumentTag marks a photo of an ID, a card or a boarding pass in
// photo-store: never linked from an entry.
const privateDocumentTag = "private-document"

// photoFolderPrefix is where this command files pictures in photo-store; a
// photo there is this command's to update.
const photoFolderPrefix = "messages/android/"

type phoneList []androidmessages.Phone

func (phones *phoneList) String() string { return fmt.Sprint(*phones) }

func (phones *phoneList) Set(value string) error {
	parts := strings.SplitN(value, "|", 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return fmt.Errorf("%q is not tag|name|folder", value)
	}
	*phones = append(*phones, androidmessages.Phone{Tag: parts[0], Name: parts[1], Dir: parts[2]})
	return nil
}

type config struct {
	phones             []androidmessages.Phone
	selfName           string
	zones              residencezones.Zones
	sourceEntry        string
	source             string
	foldIntoSource     string
	storeURL           string
	peopleStoreURL     string
	photoStoreURL      string
	photoSourceMachine string
	dryRun             bool
	batchSize          int
	show               string
}

func main() {
	var phones phoneList
	flag.Var(&phones, "phone", "a phone's export as tag|name|folder (repeat, oldest history first)")
	selfName := flag.String("self-name", "", "how the user's own messages are signed")
	timeZones := flag.String("time-zones", "", "where the user lived: first zone, then day=zone for each move (Europe/Lisbon,2021-07-11=America/Chicago)")
	sourceEntry := flag.String("source-entry", "", "the journal-store entry that describes the phones' archive, linked from every transcript")
	source := flag.String("source", "android-messages", "the import source new transcripts are filed under")
	foldIntoSource := flag.String("fold-into-source", "", "an earlier import source whose transcripts with the same source_ref are extended instead of doubled")
	storeURL := flag.String("store-url", "http://"+journalstore.DefaultListenAddress, "journal-store's base URL")
	peopleStoreURL := flag.String("people-store-url", "http://"+peoplestore.DefaultListenAddress, "people-store's base URL")
	photoStoreURL := flag.String("photo-store-url", "http://"+photostore.DefaultListenAddress, "photo-store's base URL")
	photoSourceMachine := flag.String("photo-source-machine", "", "the machine that holds the attachment files, photo-store's source_machine")
	dryRun := flag.Bool("dry-run", false, "read everything and ask the stores what they would do; write nothing")
	batchSize := flag.Int("batch", 100, "transcripts per POST /imports request")
	show := flag.String("show", "", "print the transcript with this source_ref (<numbers>/<day>), to check it")
	flag.Parse()
	if len(phones) == 0 || *selfName == "" || *timeZones == "" || *photoSourceMachine == "" || *batchSize < 1 {
		flag.Usage()
		os.Exit(2)
	}
	zones, err := residencezones.Parse(*timeZones)
	if err != nil {
		fmt.Fprintf(os.Stderr, "journal-import-android-messages: -time-zones: %v\n", err)
		os.Exit(2)
	}
	run := config{phones: phones, selfName: *selfName, zones: zones, sourceEntry: *sourceEntry, source: *source,
		foldIntoSource: *foldIntoSource, storeURL: *storeURL, peopleStoreURL: *peopleStoreURL, photoStoreURL: *photoStoreURL,
		photoSourceMachine: *photoSourceMachine, dryRun: *dryRun, batchSize: *batchSize, show: *show}
	if err := run.run(os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "journal-import-android-messages: %v\n", err)
		os.Exit(1)
	}
}

var client = &http.Client{Timeout: 5 * time.Minute}

// getJSON reads url into out; found is false on a 404.
func getJSON(address string, out any) (found bool, err error) {
	response, err := client.Get(address)
	if err != nil {
		return false, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return false, err
	}
	if response.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if response.StatusCode != http.StatusOK {
		return false, fmt.Errorf("GET %s answered %d: %s", address, response.StatusCode, bytes.TrimSpace(body))
	}
	return true, json.Unmarshal(body, out)
}

func postJSON(address string, in, out any) error {
	payload, err := json.Marshal(in)
	if err != nil {
		return err
	}
	response, err := client.Post(address, "application/json", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusCreated {
		return fmt.Errorf("POST %s answered %d: %s", address, response.StatusCode, bytes.TrimSpace(body))
	}
	return json.Unmarshal(body, out)
}

// photoState is what this run knows of one picture in photo-store.
type photoState struct {
	photo *photostore.Photo // nil when photo-store does not hold it
	// notImported says why a picture photo-store lacks cannot be imported.
	notImported string
	// thumbnail, width and height are made before any transcript is
	// written, so a transcript says the same on every run.
	thumbnail     []byte
	width, height int
}

func (run config) run(out io.Writer) error {
	archive, err := androidmessages.Read(run.phones)
	if err != nil {
		return err
	}
	for _, counts := range archive.Counts {
		fmt.Fprintf(out, "%s: read %d SMS, %d MMS, %d calls (%s to %s); dropped %d messages and %d calls another phone or row already held; left out %d drafts or failed messages, %d MMS no phone exported in full; %d attachment files missing\n",
			counts.Tag, counts.SMS, counts.MMS, counts.Calls, counts.First.Format(time.DateOnly), counts.Last.Format(time.DateOnly),
			counts.DuplicateMessages, counts.DuplicateCalls, counts.NotSentOrReceived, counts.PictureMessagesWithoutDetails, counts.AttachmentFilesMissing)
	}
	fmt.Fprintf(out, "kept %d messages and %d calls once each; the user's own numbers: %s\n", len(archive.Messages), len(archive.Calls), strings.Join(archive.SelfNumbers, ", "))
	conversations, skipped := androidmessages.Conversations(archive)
	fmt.Fprintf(out, "%d conversations kept; left out: %d with a short code or email address (%d messages), %d of more than %d people (%d messages), %d the user never wrote in (%d messages), %d messages with no one else; %d calls with no kept conversation, %d blocked calls\n",
		len(conversations), skipped.NotPhoneNumber, skipped.NotPhoneNumberMessages, skipped.TooBig, androidmessages.MaximumMembers, skipped.TooBigMessages,
		skipped.UserNeverWrote, skipped.UserNeverWroteMessages, skipped.NoOneElseMessages, skipped.CallsWithNoConversation, skipped.BlockedCalls)

	people, unknown, err := run.resolvePeople(conversations)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "people-store: %d numbers belong to %d people; %d numbers belong to no one\n", len(people), countPeople(people), len(unknown))
	if err := run.suggest(unknown, archive, conversations, out); err != nil {
		return err
	}

	photos, err := run.lookUpPhotos(conversations)
	if err != nil {
		return err
	}
	options := androidmessages.Options{SelfName: run.selfName, Zones: run.zones, SourceEntry: run.sourceEntry, Phones: run.phones,
		People: people, ContactNames: archive.ContactNames, Attachment: attachmentWriter(photos)}
	transcripts, counts := androidmessages.Transcripts(conversations, options)
	fmt.Fprintf(out, "%d transcripts hold %d messages and %d calls; %d calls on days with no texts left out; %d messages left out as a login code or password\n",
		len(transcripts), counts.Messages, counts.Calls, counts.CallsOnDaysWithoutTexts, counts.Secrets)

	for _, transcript := range transcripts {
		if transcript.Ref == run.show {
			fmt.Fprintf(out, "----- %s\n%s\n%s-----\n", transcript.Ref, transcript.Title, transcript.Body)
		}
	}

	folded, err := run.foldedRefs()
	if err != nil {
		return err
	}
	written := map[string]bool{}
	for _, transcript := range transcripts {
		written[transcript.Ref] = true
	}
	unmatched := 0
	for ref := range folded {
		if !written[ref] {
			unmatched++
		}
	}
	fmt.Fprintf(out, "%d %s transcripts are extended in place; %d of them this run would not write again\n", len(folded)-unmatched, run.foldIntoSource, unmatched)

	result, entryIDs, err := run.importTranscripts(transcripts, folded)
	if err != nil {
		return err
	}
	run.printResult(out, "transcripts", result)

	if err := run.importPhotos(transcripts, options, entryIDs, photos, out); err != nil {
		return err
	}
	// photo-store's answers change attachment lines (an id, or why there is
	// none); days whose text did not change are skipped.
	if !run.dryRun {
		again, _ := androidmessages.Transcripts(conversations, options)
		second, _, err := run.importTranscripts(again, folded)
		if err != nil {
			return err
		}
		run.printResult(out, "transcripts with their photos linked", second)
	}
	return nil
}

func (run config) printResult(out io.Writer, what string, result journalstore.ImportResult) {
	verb := "imported"
	if run.dryRun {
		verb = "dry run, nothing written; would have"
	}
	fmt.Fprintf(out, "%s %s: created %d, updated %d, unchanged %d\n", what, verb, result.Created, result.Updated, result.Skipped)
}

func countPeople(people map[string]androidmessages.Person) int {
	ids := map[string]bool{}
	for _, person := range people {
		ids[person.ID] = true
	}
	return len(ids)
}

func (run config) resolvePeople(conversations []*androidmessages.Conversation) (map[string]androidmessages.Person, []string, error) {
	people := map[string]androidmessages.Person{}
	asked := map[string]bool{}
	var unknown []string
	for _, conversation := range conversations {
		for _, number := range conversation.Others {
			if asked[number] {
				continue
			}
			asked[number] = true
			var resolution peoplestore.IdentityResolution
			found, err := getJSON(run.peopleStoreURL+"/identities/resolve?"+url.Values{"kind": {"phone"}, "value": {number}}.Encode(), &resolution)
			if err != nil {
				return nil, nil, err
			}
			if !found {
				unknown = append(unknown, number)
				continue
			}
			people[number] = androidmessages.Person{ID: resolution.Person.ID, DisplayName: resolution.Person.DisplayName}
		}
	}
	sort.Strings(unknown)
	return people, unknown, nil
}

// suggestionDetail is the source_detail of a suggestion this command makes.
type suggestionDetail struct {
	ContactNames []string `json:"contact_names"`
	Phones       []string `json:"phones"`
	MessagesFrom int      `json:"messages_from_them"`
	MessagesTo   int      `json:"messages_from_the_user"`
	FirstMessage string   `json:"first_message"`
	LastMessage  string   `json:"last_message"`
	Groups       int      `json:"group_conversations"`
}

// suggest proposes every number nobody has as a person, named as the
// phones saved it. A number any suggestion already holds is skipped (the
// store's rule for importers), and the dry run counts those itself.
func (run config) suggest(unknown []string, archive *androidmessages.Archive, conversations []*androidmessages.Conversation, out io.Writer) error {
	proposed := map[string]bool{}
	for offset := 0; ; offset += 1000 {
		var page []peoplestore.Suggestion
		if _, err := getJSON(fmt.Sprintf("%s/suggestions?limit=1000&offset=%d", run.peopleStoreURL, offset), &page); err != nil {
			return err
		}
		for _, suggestion := range page {
			for _, identity := range suggestion.Payload.Identities {
				proposed[identity.Kind+"|"+identity.Value] = true
			}
			for _, covered := range suggestion.Payload.Covered {
				proposed[covered.Kind+"|"+covered.Value] = true
			}
		}
		if len(page) < 1000 {
			break
		}
	}
	details := map[string]*suggestionDetail{}
	for _, number := range unknown {
		details[number] = &suggestionDetail{ContactNames: archive.ContactNames[number]}
	}
	for _, conversation := range conversations {
		for _, number := range conversation.Others {
			detail := details[number]
			if detail == nil {
				continue
			}
			if len(conversation.Others) > 1 {
				detail.Groups++
			}
			for _, message := range conversation.Messages {
				switch {
				case message.Sender == number:
					detail.MessagesFrom++
				case message.FromSelf && len(conversation.Others) == 1:
					detail.MessagesTo++
				default:
					continue
				}
				day := message.Time.In(run.zones.At(message.Time)).Format(time.DateOnly)
				if detail.FirstMessage == "" || day < detail.FirstMessage {
					detail.FirstMessage = day
				}
				if day > detail.LastMessage {
					detail.LastMessage = day
				}
				for _, tag := range message.Phones {
					if !contains(detail.Phones, tag) {
						detail.Phones = append(detail.Phones, tag)
					}
				}
			}
		}
	}
	made, already, named := 0, 0, 0
	for _, number := range unknown {
		if proposed["phone|"+number] {
			already++
			continue
		}
		detail := details[number]
		name := number
		if len(detail.ContactNames) > 0 {
			name = strings.Join(detail.ContactNames, " / ")
			named++
		}
		raw, err := json.Marshal(detail)
		if err != nil {
			return err
		}
		reason := fmt.Sprintf("Texted with the user from %s to %s (%d messages from this number).", detail.FirstMessage, detail.LastMessage, detail.MessagesFrom)
		if len(detail.ContactNames) > 0 {
			reason += " The phone's contact list saved it as " + name + "."
		} else {
			reason += " No contact list saved a name for it."
		}
		made++
		if run.dryRun {
			continue
		}
		var proposal peoplestore.SuggestionProposal
		if err := postJSON(run.peopleStoreURL+"/suggestions", peoplestore.SuggestionInput{
			Source: peoplestore.SuggestionSourcePhoneMessages, DisplayName: name,
			Identities: []peoplestore.IdentityInput{{Kind: "phone", Value: number, Label: "phone"}}, SourceDetail: raw, Reason: reason,
		}, &proposal); err != nil {
			return fmt.Errorf("suggestion for a number: %w", err)
		}
		if !proposal.Created {
			made--
			already++
		}
	}
	verb := "made"
	if run.dryRun {
		verb = "would make"
	}
	fmt.Fprintf(out, "people-store suggestions: %s %d (%d with a contact-list name, the rest named by the number); %d numbers already in a suggestion\n", verb, made, named, already)
	return nil
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

// lookUpPhotos asks photo-store about every picture in the kept
// conversations, by its file's hash.
func (run config) lookUpPhotos(conversations []*androidmessages.Conversation) (map[string]*photoState, error) {
	photos := map[string]*photoState{}
	for _, conversation := range conversations {
		for _, message := range conversation.Messages {
			for _, attachment := range message.Attachments {
				if !attachment.IsPicture() || attachment.Sha256 == "" || photos[attachment.Sha256] != nil {
					continue
				}
				var found []photostore.Photo
				if _, err := getJSON(run.photoStoreURL+"/photos?"+url.Values{"sha256": {attachment.Sha256}, "include_deleted": {"true"}}.Encode(), &found); err != nil {
					return nil, err
				}
				state := &photoState{}
				if len(found) > 0 {
					state.photo = &found[0]
				}
				if attachment.Path != "" && (state.photo == nil || strings.HasPrefix(state.photo.Folder, photoFolderPrefix)) {
					state.thumbnail, state.width, state.height, state.notImported = makeThumbnail(attachment)
				}
				photos[attachment.Sha256] = state
			}
		}
	}
	return photos, nil
}

func pictureWord(contentType string) string {
	if strings.HasPrefix(contentType, "video/") {
		return "video"
	}
	return "photo"
}

// attachmentWriter writes an attachment as a photo-store chip, or as a note
// that says why there is none.
func attachmentWriter(photos map[string]*photoState) func(*androidmessages.Message, androidmessages.Attachment) string {
	return func(_ *androidmessages.Message, attachment androidmessages.Attachment) string {
		contentType := attachment.ContentType
		switch {
		case attachment.IsPicture():
			word := pictureWord(contentType)
			if attachment.Path == "" {
				return "_" + word + ", not copied off the phone_"
			}
			state := photos[attachment.Sha256]
			if state == nil || state.photo == nil {
				if state != nil && state.notImported != "" {
					return fmt.Sprintf("_%s, not in photo-store: %s_", word, state.notImported)
				}
				return "_" + word + ", not in photo-store yet_"
			}
			photo := state.photo
			switch {
			case photo.PurgedAt != 0 || photo.DeletedAt != 0:
				return "_" + word + ", removed from photo-store_"
			case contains(photo.Tags, privateDocumentTag):
				return "_" + word + " of a private document, not linked_"
			}
			return fmt.Sprintf("[%s](ref:%s)", word, photo.ID)
		case strings.HasPrefix(contentType, "audio/"):
			return "_voice message_"
		case strings.Contains(contentType, "vcard"):
			return "_contact card_"
		case contentType == "application/pdf":
			return "_PDF file_"
		case contentType == "":
			return "_attachment_"
		}
		return "_attachment (" + contentType + ")_"
	}
}

// foldedRefs are the source_refs the earlier import holds, deleted ones too
// (an import never brings a deleted entry back, but it must not double it).
func (run config) foldedRefs() (map[string]bool, error) {
	refs := map[string]bool{}
	if run.foldIntoSource == "" {
		return refs, nil
	}
	for offset := 0; ; offset += 500 {
		var page []journalstore.Entry
		query := url.Values{"source": {run.foldIntoSource}, "include_deleted": {"true"}, "limit": {"500"}, "offset": {fmt.Sprint(offset)}}
		if _, err := getJSON(run.storeURL+"/entries?"+query.Encode(), &page); err != nil {
			return nil, err
		}
		for _, entry := range page {
			if entry.Source != run.foldIntoSource {
				return nil, fmt.Errorf("journal-store answered ?source=%s with an entry of source %q: it is too old to filter by source", run.foldIntoSource, entry.Source)
			}
			refs[entry.SourceRef] = true
		}
		if len(page) < 500 {
			break
		}
	}
	return refs, nil
}

func (run config) importTranscripts(transcripts []androidmessages.Transcript, folded map[string]bool) (journalstore.ImportResult, map[string]string, error) {
	entries := make([]journalstore.ImportEntry, 0, len(transcripts))
	for _, transcript := range transcripts {
		source := run.source
		if folded[transcript.Ref] {
			source = run.foldIntoSource
		}
		entries = append(entries, journalstore.ImportEntry{
			Source: source, SourceRef: transcript.Ref, SourceHash: transcript.Hash,
			Kind: journalstore.KindTranscript, Visibility: journalstore.VisibilityPrivate,
			Title: transcript.Title, Body: transcript.Body, WrittenAt: transcript.FirstTime.Unix(), Tags: transcript.Tags,
		})
	}
	entryIDs := map[string]string{}
	total, err := importclient.PostAll(run.storeURL, entries, run.batchSize, run.dryRun, func(outcome journalstore.ImportOutcome) {
		entryIDs[outcome.SourceRef] = outcome.EntryID
	})
	return total, entryIDs, err
}

// importPhotos sends photo-store every picture it lacks, or holds in this
// command's folders, cited by the transcript of the day it was sent.
func (run config) importPhotos(transcripts []androidmessages.Transcript, options androidmessages.Options, entryIDs map[string]string, photos map[string]*photoState, out io.Writer) error {
	var batch []photostore.ImportPhoto
	var shas []string
	skipped := map[string]int{}
	cited := 0
	queued := map[string]bool{}
	for _, transcript := range transcripts {
		folder := photoFolderPrefix + strings.ReplaceAll(options.ConversationName(transcript.Conversation), "/", "-")
		for _, message := range transcript.Messages {
			for _, attachment := range message.Attachments {
				if !attachment.IsPicture() || attachment.Path == "" || queued[attachment.Sha256] {
					continue
				}
				queued[attachment.Sha256] = true
				state := photos[attachment.Sha256]
				if state.photo != nil && !strings.HasPrefix(state.photo.Folder, photoFolderPrefix) {
					cited++
					continue
				}
				if state.photo != nil && state.photo.PurgedAt != 0 {
					skipped["purged in photo-store"]++
					continue
				}
				if state.notImported != "" {
					skipped[state.notImported]++
					continue
				}
				thumb, width, height := state.thumbnail, state.width, state.height
				entryID := entryIDs[transcript.Ref]
				if entryID == "" && !run.dryRun {
					return fmt.Errorf("journal-store gave no entry id for %s", transcript.Ref)
				}
				batch = append(batch, photostore.ImportPhoto{Sha256: attachment.Sha256, SourceMachine: run.photoSourceMachine,
					SourcePath: entryID, Folder: folder, MediaType: attachment.ContentType, TakenAt: message.Time.Unix(),
					DateSource: photostore.DateFromMessage, Width: width, Height: height, ThumbnailJPEG: thumb})
				shas = append(shas, attachment.Sha256)
			}
		}
	}
	reasons := make([]string, 0, len(skipped))
	for reason, count := range skipped {
		reasons = append(reasons, fmt.Sprintf("%d %s", count, reason))
	}
	sort.Strings(reasons)
	fmt.Fprintf(out, "photo-store: %d pictures photo-store already held from elsewhere, cited as they are; %d to import or update; not imported: %s\n", cited, len(batch), strings.Join(reasons, ", "))
	if run.dryRun {
		return nil
	}
	var total photostore.ImportResult
	for start := 0; start < len(batch); start += 50 {
		end := min(start+50, len(batch))
		var result photostore.ImportResult
		if err := postJSON(run.photoStoreURL+"/imports", photostore.ImportRequest{Photos: batch[start:end]}, &result); err != nil {
			return fmt.Errorf("photos %d–%d: %w (the batches before it were written)", start+1, end, err)
		}
		if len(result.IDs) != end-start {
			return fmt.Errorf("photo-store answered %d ids for %d photos", len(result.IDs), end-start)
		}
		for index, id := range result.IDs {
			state := photos[shas[start+index]]
			var photo photostore.Photo
			if _, err := getJSON(run.photoStoreURL+"/photos/"+id, &photo); err != nil {
				return err
			}
			state.photo = &photo
		}
		total.Created += result.Created
		total.Updated += result.Updated
		total.Skipped += result.Skipped
	}
	fmt.Fprintf(out, "photo-store imported: created %d, updated %d, unchanged %d\n", total.Created, total.Updated, total.Skipped)
	return nil
}

// makeThumbnail makes photo-store's thumbnail of a picture: straight from an
// image Go reads, else from the first frame ffmpeg draws (videos, BMP). The
// reason is why there is none.
func makeThumbnail(attachment androidmessages.Attachment) (thumb []byte, width, height int, reason string) {
	original, err := os.ReadFile(attachment.Path)
	if err != nil {
		return nil, 0, 0, "unreadable file: " + err.Error()
	}
	if thumb, width, height, err = thumbnail.Make(original); err == nil {
		return thumb, width, height, ""
	}
	frame, err := firstFrame(attachment.Path)
	if err != nil {
		return nil, 0, 0, "no decoder on this host for " + attachment.ContentType
	}
	if thumb, width, height, err = thumbnail.Make(frame); err != nil {
		return nil, 0, 0, "no decoder on this host for " + attachment.ContentType
	}
	return thumb, width, height, ""
}

func firstFrame(path string) ([]byte, error) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		return nil, err
	}
	var frame, errorOutput bytes.Buffer
	command := exec.Command(ffmpeg, "-v", "error", "-i", path, "-frames:v", "1", "-f", "image2", "-c:v", "mjpeg", "pipe:1")
	command.Stdout, command.Stderr = &frame, &errorOutput
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("%w: %s", err, bytes.TrimSpace(errorOutput.Bytes()))
	}
	if frame.Len() == 0 {
		return nil, errors.New("ffmpeg drew no frame")
	}
	return frame.Bytes(), nil
}

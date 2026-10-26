// Package facebookmessages reads the Messenger part of a Facebook data
// export (your_facebook_activity/messages, JSON format) and turns its small
// chats into transcripts: one per thread per day, both sides.
//
// A thread is kept only when it has at most MaximumParticipants people: DMs
// and small chats are kept, big groups are not.
package facebookmessages

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kayushkin/journal-store/internal/facebookexport"
	"github.com/kayushkin/journal-store/internal/residencezones"
	"github.com/kayushkin/journal-store/internal/transcripttext"
)

// MaximumParticipants is the most people, the user included, a thread may
// have to be imported.
const MaximumParticipants = 8

// Folders are the folders under messages/ that hold threads.
var Folders = []string{"inbox", "archived_threads", "filtered_threads", "message_requests", "e2ee_cutover"}

// Thread is one conversation folder: messages/<folder>/<thread>/message_*.json.
type Thread struct {
	Folder       string // inbox, archived_threads, …
	ID           string // the thread folder's name
	Title        string
	Participants []string
	// ParticipantCount is how many people the export lists, two of the same
	// name counted twice.
	ParticipantCount int
	Messages         []Message
}

// Message is one message, with its text already repaired (see repair).
type Message struct {
	Sender  string
	Time    time.Time
	Content string
	// Notes are what the message carried besides text, one per line:
	// "photo: name.jpg", "call, 4 min", "link: https://…".
	Notes []string
}

type threadFile struct {
	Title        string `json:"title"`
	Participants []struct {
		Name string `json:"name"`
	} `json:"participants"`
	Messages []messageFile `json:"messages"`
}

type uriFile struct {
	URI string `json:"uri"`
}

type messageFile struct {
	SenderName   string    `json:"sender_name"`
	TimestampMs  int64     `json:"timestamp_ms"`
	Content      string    `json:"content"`
	Photos       []uriFile `json:"photos"`
	Videos       []uriFile `json:"videos"`
	Gifs         []uriFile `json:"gifs"`
	Files        []uriFile `json:"files"`
	AudioFiles   []uriFile `json:"audio_files"`
	Sticker      *uriFile  `json:"sticker"`
	CallDuration *int      `json:"call_duration"`
	Missed       bool      `json:"missed"`
	IsUnsent     bool      `json:"is_unsent"`
	Share        *struct {
		Link string `json:"link"`
	} `json:"share"`
	Reactions []struct {
		Reaction string `json:"reaction"`
		Actor    string `json:"actor"`
	} `json:"reactions"`
}

// Skipped counts the threads Read left out.
type Skipped struct {
	TooManyParticipants int
	Messages            int // in those threads
}

// Read reads every thread of at most MaximumParticipants people under
// messagesDir (the export's your_facebook_activity/messages).
func Read(messagesDir string) ([]Thread, Skipped, error) {
	var threads []Thread
	var skipped Skipped
	for _, folder := range Folders {
		entries, err := os.ReadDir(filepath.Join(messagesDir, folder))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, skipped, err
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			thread, err := readThread(filepath.Join(messagesDir, folder, entry.Name()))
			if err != nil {
				return nil, skipped, err
			}
			if len(thread.Messages) == 0 {
				continue
			}
			thread.Folder, thread.ID = folder, entry.Name()
			if thread.ParticipantCount > MaximumParticipants {
				skipped.TooManyParticipants++
				skipped.Messages += len(thread.Messages)
				continue
			}
			threads = append(threads, thread)
		}
	}
	return threads, skipped, nil
}

func readThread(directory string) (Thread, error) {
	files, err := filepath.Glob(filepath.Join(directory, "message_*.json"))
	if err != nil {
		return Thread{}, err
	}
	sort.Strings(files)
	var thread Thread
	seenParticipant := map[string]bool{}
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			return Thread{}, err
		}
		var parsed threadFile
		if err := json.Unmarshal(raw, &parsed); err != nil {
			return Thread{}, fmt.Errorf("%s: %w", file, err)
		}
		thread.Title = facebookexport.Repair(parsed.Title)
		thread.ParticipantCount = max(thread.ParticipantCount, len(parsed.Participants))
		for _, participant := range parsed.Participants {
			name := facebookexport.Repair(participant.Name)
			if !seenParticipant[name] {
				seenParticipant[name] = true
				thread.Participants = append(thread.Participants, name)
			}
		}
		for _, message := range parsed.Messages {
			thread.Messages = append(thread.Messages, convert(message))
		}
	}
	return thread, nil
}

func baseNames(label string, files []uriFile) []string {
	var notes []string
	for _, file := range files {
		notes = append(notes, label+": "+path.Base(file.URI))
	}
	return notes
}

func convert(message messageFile) Message {
	converted := Message{
		Sender:  facebookexport.Repair(message.SenderName),
		Time:    time.UnixMilli(message.TimestampMs).UTC(),
		Content: facebookexport.Repair(message.Content),
	}
	converted.Notes = append(converted.Notes, baseNames("photo", message.Photos)...)
	converted.Notes = append(converted.Notes, baseNames("video", message.Videos)...)
	converted.Notes = append(converted.Notes, baseNames("gif", message.Gifs)...)
	converted.Notes = append(converted.Notes, baseNames("file", message.Files)...)
	converted.Notes = append(converted.Notes, baseNames("voice message", message.AudioFiles)...)
	if message.Sticker != nil {
		converted.Notes = append(converted.Notes, "sticker")
	}
	switch {
	case message.CallDuration != nil && message.Missed:
		converted.Notes = append(converted.Notes, "missed call")
	case message.CallDuration != nil:
		converted.Notes = append(converted.Notes, fmt.Sprintf("call, %d min", (*message.CallDuration+59)/60))
	}
	if message.Share != nil && message.Share.Link != "" && !strings.Contains(converted.Content, message.Share.Link) {
		converted.Notes = append(converted.Notes, "link: "+facebookexport.Repair(message.Share.Link))
	}
	if message.IsUnsent {
		converted.Notes = append(converted.Notes, "unsent")
	}
	for _, reaction := range message.Reactions {
		converted.Notes = append(converted.Notes, fmt.Sprintf("%s reacted %s", facebookexport.Repair(reaction.Actor), facebookexport.Repair(reaction.Reaction)))
	}
	return converted
}

// Options say how transcripts are written.
type Options struct {
	// SelfName is the user's own name in the export, left out of "with".
	SelfName string
	// People maps a name as the export writes it to a people-store id.
	People map[string]string
	// SourceEntry is the journal-store entry that describes the export.
	SourceEntry string
	Zones       residencezones.Zones
}

// Transcript is one thread's messages on one day.
type Transcript struct {
	Folder    string
	ThreadID  string
	Day       string
	Title     string
	Body      string
	Hash      string
	FirstTime time.Time
}

// Transcripts groups every thread's messages by day, oldest first.
func Transcripts(threads []Thread, options Options) []Transcript {
	var transcripts []Transcript
	for _, thread := range threads {
		byDay := map[string][]Message{}
		for _, message := range thread.Messages {
			day := message.Time.In(options.Zones.At(message.Time)).Format(time.DateOnly)
			byDay[day] = append(byDay[day], message)
		}
		days := make([]string, 0, len(byDay))
		for day := range byDay {
			days = append(days, day)
		}
		sort.Strings(days)
		for _, day := range days {
			messages := byDay[day]
			sort.SliceStable(messages, func(i, j int) bool { return messages[i].Time.Before(messages[j].Time) })
			body := transcriptBody(thread, messages, options)
			digest := sha256.Sum256([]byte(body))
			transcripts = append(transcripts, Transcript{
				Folder: thread.Folder, ThreadID: thread.ID, Day: day,
				Title: fmt.Sprintf("Messenger: %s, %s", threadTitle(thread, options), day),
				Body:  body, Hash: hex.EncodeToString(digest[:]), FirstTime: messages[0].Time,
			})
		}
	}
	return transcripts
}

func otherParticipants(thread Thread, options Options) []string {
	var others []string
	for _, participant := range thread.Participants {
		if participant != options.SelfName {
			others = append(others, participant)
		}
	}
	return others
}

func threadTitle(thread Thread, options Options) string {
	if thread.Title != "" && thread.Title != options.SelfName {
		return thread.Title
	}
	if others := otherParticipants(thread, options); len(others) > 0 {
		return strings.Join(others, ", ")
	}
	return thread.ID
}

func participantLabel(name string, options Options) string {
	if personID := options.People[name]; personID != "" {
		return fmt.Sprintf("[%s](ref:%s)", transcripttext.MarkdownSafe(name), personID)
	}
	return transcripttext.MarkdownSafe(name)
}

func transcriptBody(thread Thread, messages []Message, options Options) string {
	var body strings.Builder
	first := messages[0].Time.In(options.Zones.At(messages[0].Time))
	fmt.Fprintf(&body, "Facebook Messenger, %s (times %s).", first.Format("Monday, January 2, 2006"), first.Format("MST"))
	if options.SourceEntry != "" {
		fmt.Fprintf(&body, " From the [Facebook export](ref:%s).", options.SourceEntry)
	}
	fmt.Fprintf(&body, " Thread `%s/%s`.\n\n", thread.Folder, thread.ID)
	var labels []string
	for _, participant := range otherParticipants(thread, options) {
		labels = append(labels, participantLabel(participant, options))
	}
	fmt.Fprintf(&body, "With: %s\n\n", strings.Join(labels, ", "))
	for _, message := range messages {
		clock := message.Time.In(options.Zones.At(message.Time)).Format("15:04")
		lines := strings.Split(strings.TrimSpace(message.Content), "\n")
		speaker := transcripttext.MarkdownSafe(message.Sender)
		if lines[0] == "" && len(message.Notes) > 0 {
			fmt.Fprintf(&body, "- %s **%s**: _%s_\n", clock, speaker, transcripttext.MarkdownSafe(message.Notes[0]))
			for _, note := range message.Notes[1:] {
				fmt.Fprintf(&body, "  _%s_\n", transcripttext.MarkdownSafe(note))
			}
			continue
		}
		fmt.Fprintf(&body, "- %s **%s**: %s\n", clock, speaker, transcripttext.MarkdownSafe(lines[0]))
		for _, more := range lines[1:] {
			fmt.Fprintf(&body, "  %s\n", transcripttext.MarkdownSafe(more))
		}
		for _, note := range message.Notes {
			fmt.Fprintf(&body, "  _%s_\n", transcripttext.MarkdownSafe(note))
		}
	}
	return body.String()
}

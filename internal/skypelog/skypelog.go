// Package skypelog reads a NirSoft SkypeLogView text export and turns it into
// transcripts: one journal-store entry per conversation per day, every line
// carrying the log's own Record Number so a citation can be found again in the
// file.
//
// The export is a run of records, each between two lines of 50 "=" signs,
// with ten "Label : value" fields; a chat message may run onto more lines
// before the ChatID field. Times carry no zone: they are the clock of the
// machine that exported them, which the caller names.
package skypelog

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"github.com/kayushkin/journal-store/internal/transcripttext"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Record is one record of the export.
type Record struct {
	Number      int
	ActionType  string
	Time        time.Time // zero when the record carries no time
	UserName    string
	DisplayName string
	Duration    string
	Message     string
	ChatID      string
}

const separator = "=================================================="

// fieldLabels are the export's labels, in the order a record holds them.
var fieldLabels = []string{"Record Number", "Action Type", "Action Time", "End Time", "User Name",
	"Display Name", "Duration", "Chat Message", "ChatID", "Filename"}

// timeLayout is how SkypeLogView writes a time: 9/14/2015 8:05:31 PM.
const timeLayout = "1/2/2006 3:04:05 PM"

// labelOf returns the field a line starts, and its value, when the line is
// "Label<spaces>: value" for one of the known labels.
func labelOf(line string) (string, string, bool) {
	for _, label := range fieldLabels {
		rest, found := strings.CutPrefix(line, label)
		if !found {
			continue
		}
		trimmed := strings.TrimLeft(rest, " ")
		value, found := strings.CutPrefix(trimmed, ":")
		if !found {
			continue
		}
		return label, strings.TrimPrefix(value, " "), true
	}
	return "", "", false
}

// Parse reads the whole export. location is the zone the export's clock was
// in. A record whose time does not parse is kept with a zero time; one whose
// Record Number does not parse is an error, because every citation rests on it.
func Parse(reader io.Reader, location *time.Location) ([]Record, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	var records []Record
	var current *Record
	var currentLabel string
	lineNumber := 0
	finish := func() error {
		if current == nil {
			return nil
		}
		current.Message = strings.TrimRight(current.Message, "\n")
		records = append(records, *current)
		current = nil
		return nil
	}
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimRight(strings.TrimPrefix(scanner.Text(), "\uFEFF"), "\r")
		if line == separator {
			if current != nil && currentLabel == "Filename" {
				if err := finish(); err != nil {
					return nil, err
				}
			}
			continue
		}
		label, value, isField := labelOf(line)
		if isField && label == "Record Number" {
			if err := finish(); err != nil {
				return nil, err
			}
			number, err := strconv.Atoi(strings.TrimSpace(value))
			if err != nil {
				return nil, fmt.Errorf("line %d: record number %q: %w", lineNumber, value, err)
			}
			current = &Record{Number: number}
			currentLabel = label
			continue
		}
		if current == nil {
			continue
		}
		if isField && (currentLabel != "Chat Message" || label == "ChatID") {
			currentLabel = label
			switch label {
			case "Action Type":
				current.ActionType = value
			case "Action Time":
				if parsed, err := time.ParseInLocation(timeLayout, strings.TrimSpace(value), location); err == nil {
					current.Time = parsed
				}
			case "User Name":
				current.UserName = strings.TrimSpace(value)
			case "Display Name":
				current.DisplayName = strings.TrimSpace(value)
			case "Duration":
				current.Duration = strings.TrimSpace(value)
			case "Chat Message":
				current.Message = value
			case "ChatID":
				current.ChatID = strings.TrimSpace(value)
			}
			continue
		}
		if currentLabel == "Chat Message" {
			// A message runs onto more lines until the ChatID field.
			current.Message += "\n" + line
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if err := finish(); err != nil {
		return nil, err
	}
	return records, nil
}

// Transcript is one conversation's records on one day.
type Transcript struct {
	ConversationKey string // the ChatID, or the other person's name for a call with none
	Day             string // YYYY-MM-DD in the export's zone
	Title           string
	Records         []Record
	Body            string
	Hash            string
	FirstTime       time.Time
}

// Options say how transcripts are written.
type Options struct {
	// Self is the user's own Skype name; a one-to-one chat is named by the
	// other side.
	Self string
	// People maps a Skype name to a people-store id, drawn as a chip.
	People map[string]string
	// SourceEntry is the journal-store entry that describes the log.
	SourceEntry string
	// EarliestYear drops records dated before it: a call in an export can
	// carry a bad clock.
	EarliestYear int
}

// Skipped counts what Transcripts left out, and why.
type Skipped struct {
	NoTime     int
	BadClock   int
	EmptyChats int
}

// conversationKey is what groups a record: its ChatID, or for a call with
// none, the person on the other end.
func conversationKey(record Record, self string) string {
	if record.ChatID != "" {
		return record.ChatID
	}
	if record.UserName != "" && record.UserName != self {
		return record.UserName
	}
	return "calls"
}

// otherSide is the one-to-one partner a ChatID names, or "" for a group.
func otherSide(key, self string) string {
	if strings.HasPrefix(key, "19:") || key == "calls" {
		return ""
	}
	if strings.HasPrefix(key, "#") {
		// #nameA/$nameB;hash
		pair := strings.TrimPrefix(key, "#")
		if semicolon := strings.Index(pair, ";"); semicolon >= 0 {
			pair = pair[:semicolon]
		}
		for _, name := range strings.Split(pair, "/") {
			name = strings.TrimPrefix(name, "$")
			if name != self && name != "" {
				return name
			}
		}
		return ""
	}
	return key
}

// Transcripts groups records by conversation and day and writes each group's
// body. Chat messages, calls and file transfers are kept; records with an
// empty message and no duration (Skype's own "Chat" markers) are not.
func Transcripts(records []Record, location *time.Location, options Options) ([]Transcript, Skipped) {
	var skipped Skipped
	displayNames := map[string]map[string]int{}
	sendersByConversation := map[string]map[string]int{}
	groups := map[string]*Transcript{}
	var order []string
	for _, record := range records {
		if record.Time.IsZero() {
			skipped.NoTime++
			continue
		}
		if record.Time.Year() < options.EarliestYear {
			skipped.BadClock++
			continue
		}
		if strings.TrimSpace(record.Message) == "" && record.Duration == "" {
			skipped.EmptyChats++
			continue
		}
		if record.UserName != "" && record.DisplayName != "" {
			if displayNames[record.UserName] == nil {
				displayNames[record.UserName] = map[string]int{}
			}
			displayNames[record.UserName][record.DisplayName]++
		}
		key := conversationKey(record, options.Self)
		if sendersByConversation[key] == nil {
			sendersByConversation[key] = map[string]int{}
		}
		if record.UserName != "" && record.UserName != options.Self {
			sendersByConversation[key][record.UserName]++
		}
		day := record.Time.In(location).Format("2006-01-02")
		groupKey := key + "/" + day
		group := groups[groupKey]
		if group == nil {
			group = &Transcript{ConversationKey: key, Day: day, FirstTime: record.Time}
			groups[groupKey] = group
			order = append(order, groupKey)
		}
		group.Records = append(group.Records, record)
	}
	nameOf := func(user string) string {
		best, bestCount := user, 0
		for name, count := range displayNames[user] {
			if count > bestCount || (count == bestCount && name < best) {
				best, bestCount = name, count
			}
		}
		return best
	}
	titleOf := func(key string) string {
		if other := otherSide(key, options.Self); other != "" {
			return nameOf(other)
		}
		if key == "calls" {
			return "calls"
		}
		type sender struct {
			user  string
			count int
		}
		var senders []sender
		for user, count := range sendersByConversation[key] {
			senders = append(senders, sender{user, count})
		}
		sort.Slice(senders, func(i, j int) bool {
			if senders[i].count != senders[j].count {
				return senders[i].count > senders[j].count
			}
			return senders[i].user < senders[j].user
		})
		var names []string
		for _, s := range senders[:min(3, len(senders))] {
			names = append(names, nameOf(s.user))
		}
		label := "group with " + strings.Join(names, ", ")
		if len(senders) > 3 {
			label += fmt.Sprintf(" and %d others", len(senders)-3)
		}
		return label
	}
	sort.Strings(order)
	transcripts := make([]Transcript, 0, len(order))
	for _, groupKey := range order {
		group := groups[groupKey]
		sort.SliceStable(group.Records, func(i, j int) bool {
			if !group.Records[i].Time.Equal(group.Records[j].Time) {
				return group.Records[i].Time.Before(group.Records[j].Time)
			}
			return group.Records[i].Number < group.Records[j].Number
		})
		group.FirstTime = group.Records[0].Time
		group.Title = fmt.Sprintf("Skype: %s, %s", titleOf(group.ConversationKey), group.Day)
		group.Body = transcriptBody(group, location, options, nameOf)
		digest := sha256.Sum256([]byte(group.Body))
		group.Hash = hex.EncodeToString(digest[:])
		transcripts = append(transcripts, *group)
	}
	return transcripts, skipped
}

func transcriptBody(group *Transcript, location *time.Location, options Options, nameOf func(string) string) string {
	var body strings.Builder
	first := group.Records[0].Time.In(location)
	fmt.Fprintf(&body, "Skype, %s (times %s).", first.Format("Monday, January 2, 2006"), first.Format("MST"))
	if options.SourceEntry != "" {
		fmt.Fprintf(&body, " From the [Skype log](ref:%s);", options.SourceEntry)
	}
	fmt.Fprintf(&body, " records %d to %d. Conversation `%s`.\n\n", lowestNumber(group.Records), highestNumber(group.Records), group.ConversationKey)
	var participants []string
	seen := map[string]bool{}
	for _, record := range group.Records {
		if record.UserName == "" || seen[record.UserName] {
			continue
		}
		seen[record.UserName] = true
		name := nameOf(record.UserName)
		if id, known := options.People[record.UserName]; known {
			participants = append(participants, fmt.Sprintf("[%s](ref:%s) (`%s`)", name, id, record.UserName))
		} else {
			participants = append(participants, fmt.Sprintf("%s (`%s`)", transcripttext.MarkdownSafe(name), record.UserName))
		}
	}
	fmt.Fprintf(&body, "With: %s\n\n", strings.Join(participants, ", "))
	for _, record := range group.Records {
		speaker := transcripttext.MarkdownSafe(record.DisplayName)
		if speaker == "" {
			speaker = record.UserName
		}
		clock := record.Time.In(location).Format("15:04")
		message := strings.TrimSpace(record.Message)
		switch {
		case record.ActionType == "Chat Message" || message != "":
			lines := strings.Split(message, "\n")
			fmt.Fprintf(&body, "- `%d` %s **%s**: %s\n", record.Number, clock, speaker, transcripttext.MarkdownSafe(lines[0]))
			for _, more := range lines[1:] {
				fmt.Fprintf(&body, "  %s\n", transcripttext.MarkdownSafe(more))
			}
		default:
			fmt.Fprintf(&body, "- `%d` %s **%s**: _%s, %s_\n", record.Number, clock, speaker, strings.ToLower(record.ActionType), record.Duration)
		}
	}
	return body.String()
}

func lowestNumber(records []Record) int {
	lowest := records[0].Number
	for _, record := range records {
		lowest = min(lowest, record.Number)
	}
	return lowest
}

func highestNumber(records []Record) int {
	highest := records[0].Number
	for _, record := range records {
		highest = max(highest, record.Number)
	}
	return highest
}

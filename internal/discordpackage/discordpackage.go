// Package discordpackage reads the Messages folder of a Discord data package
// (the export Discord emails on request) and turns its direct messages and
// group direct messages into transcripts: one per channel per day.
//
// The package holds only the account owner's own messages, so a transcript
// is one side of a conversation, unless the channel's whole history was also
// fetched from Discord (ReadFullHistory), in which case it has both. Server
// channels are left out: only DMs and small group chats are kept.
package discordpackage

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kayushkin/journal-store/internal/residencezones"
	"github.com/kayushkin/journal-store/internal/transcripttext"
)

// The channel types that are conversations with people rather than server
// channels.
const (
	TypeDirectMessage      = "DM"
	TypeGroupDirectMessage = "GROUP_DM"
)

// timestampLayout is how the package writes a message's time, in UTC.
const timestampLayout = "2006-01-02 15:04:05"

// directMessagePrefix starts index.json's name for a DM, before the other
// side's username.
const directMessagePrefix = "Direct Message with "

// Channel is one channel folder: c<id>/channel.json and messages.json, named
// from index.json.
type Channel struct {
	ID         string
	Type       string
	IndexName  string   // index.json's name: "Direct Message with <username>", a group's name, or "None"
	Recipients []string // Discord user ids, the owner's among them
	Messages   []Message
	// FullHistoryFetchedAt is when the channel's whole history, both sides,
	// was fetched from Discord; zero when only the package's side is known.
	FullHistoryFetchedAt time.Time
}

// Message is one message in the channel. A message from the package has no
// author: the owner sent it.
type Message struct {
	ID          string
	Time        time.Time
	Contents    string
	Attachments []string // CDN links, or file names
	AuthorID    string
	AuthorName  string
	// Event names what a message with no text was, such as a call, from
	// Discord's message type.
	Event string
}

type channelFile struct {
	ID         string   `json:"id"`
	Type       string   `json:"type"`
	Recipients []string `json:"recipients"`
}

type messageFile struct {
	ID          json.Number `json:"ID"`
	Timestamp   string      `json:"Timestamp"`
	Contents    string      `json:"Contents"`
	Attachments string      `json:"Attachments"`
}

// Read reads every DM and group DM channel in the Messages folder, and how
// many server channels it left out.
func Read(messagesDir string) ([]Channel, int, error) {
	raw, err := os.ReadFile(filepath.Join(messagesDir, "index.json"))
	if err != nil {
		return nil, 0, err
	}
	index := map[string]*string{}
	if err := json.Unmarshal(raw, &index); err != nil {
		return nil, 0, fmt.Errorf("index.json: %w", err)
	}
	ids := make([]string, 0, len(index))
	for id := range index {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var channels []Channel
	leftOut := 0
	for _, id := range ids {
		folder := filepath.Join(messagesDir, "c"+id)
		raw, err := os.ReadFile(filepath.Join(folder, "channel.json"))
		if err != nil {
			return nil, 0, err
		}
		var described channelFile
		if err := json.Unmarshal(raw, &described); err != nil {
			return nil, 0, fmt.Errorf("c%s/channel.json: %w", id, err)
		}
		if described.Type != TypeDirectMessage && described.Type != TypeGroupDirectMessage {
			leftOut++
			continue
		}
		raw, err = os.ReadFile(filepath.Join(folder, "messages.json"))
		if err != nil {
			return nil, 0, err
		}
		var sent []messageFile
		if err := json.Unmarshal(raw, &sent); err != nil {
			return nil, 0, fmt.Errorf("c%s/messages.json: %w", id, err)
		}
		channel := Channel{ID: described.ID, Type: described.Type, Recipients: described.Recipients}
		if index[id] != nil {
			channel.IndexName = *index[id]
		}
		for _, message := range sent {
			at, err := time.ParseInLocation(timestampLayout, message.Timestamp, time.UTC)
			if err != nil {
				return nil, 0, fmt.Errorf("c%s message %s: %w", id, message.ID, err)
			}
			channel.Messages = append(channel.Messages, Message{
				ID: message.ID.String(), Time: at, Contents: message.Contents,
				Attachments: strings.Fields(message.Attachments),
			})
		}
		channels = append(channels, channel)
	}
	return channels, leftOut, nil
}

// fetchedMessage is one line of a history fetch: a raw Discord message object.
type fetchedMessage struct {
	ID        string `json:"id"`
	Type      int    `json:"type"`
	Timestamp string `json:"timestamp"`
	Content   string `json:"content"`
	Author    struct {
		ID         string `json:"id"`
		Username   string `json:"username"`
		GlobalName string `json:"global_name"`
	} `json:"author"`
	Attachments []struct {
		Filename string `json:"filename"`
	} `json:"attachments"`
}

// eventNames are Discord's message types that carry no text of their own.
var eventNames = map[int]string{
	1: "added someone to the group", 2: "removed someone from the group", 3: "call",
	4: "renamed the group", 5: "changed the group icon", 6: "pinned a message",
}

// ReadFullHistory reads the channels in a history fetch folder
// (multichat-fetch-discord-history -direct-message): <channel id>.jsonl, used
// only once <channel id>.done marks it complete. It returns each channel's
// messages and when the fetch finished.
func ReadFullHistory(folder string) (map[string][]Message, map[string]time.Time, error) {
	histories := map[string][]Message{}
	fetchedAt := map[string]time.Time{}
	markers, err := filepath.Glob(filepath.Join(folder, "*.done"))
	if err != nil {
		return nil, nil, err
	}
	for _, marker := range markers {
		channelID := strings.TrimSuffix(filepath.Base(marker), ".done")
		raw, err := os.ReadFile(marker)
		if err != nil {
			return nil, nil, err
		}
		finished, err := time.Parse(time.RFC3339, strings.TrimSpace(string(raw)))
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", marker, err)
		}
		raw, err = os.ReadFile(filepath.Join(folder, channelID+".jsonl"))
		if err != nil {
			return nil, nil, err
		}
		var messages []Message
		for number, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			if line == "" {
				continue
			}
			var fetched fetchedMessage
			if err := json.Unmarshal([]byte(line), &fetched); err != nil {
				return nil, nil, fmt.Errorf("%s.jsonl line %d: %w", channelID, number+1, err)
			}
			at, err := time.Parse(time.RFC3339Nano, fetched.Timestamp)
			if err != nil {
				return nil, nil, fmt.Errorf("%s.jsonl line %d: %w", channelID, number+1, err)
			}
			message := Message{ID: fetched.ID, Time: at.UTC(), Contents: fetched.Content,
				AuthorID: fetched.Author.ID, AuthorName: fetched.Author.GlobalName}
			if message.AuthorName == "" {
				message.AuthorName = fetched.Author.Username
			}
			for _, attachment := range fetched.Attachments {
				message.Attachments = append(message.Attachments, attachment.Filename)
			}
			if strings.TrimSpace(fetched.Content) == "" && len(fetched.Attachments) == 0 {
				message.Event = eventNames[fetched.Type]
			}
			messages = append(messages, message)
		}
		histories[channelID] = messages
		fetchedAt[channelID] = finished
	}
	return histories, fetchedAt, nil
}

// MergeFullHistory adds each fetched history to its channel: every fetched
// message, plus any of the package's that the fetch lacks (deleted since).
// It returns how many channels gained a full history.
func MergeFullHistory(channels []Channel, histories map[string][]Message, fetchedAt map[string]time.Time) int {
	merged := 0
	for index := range channels {
		history, found := histories[channels[index].ID]
		if !found {
			continue
		}
		fetchedIDs := map[string]bool{}
		for _, message := range history {
			fetchedIDs[message.ID] = true
		}
		combined := append([]Message(nil), history...)
		for _, message := range channels[index].Messages {
			if !fetchedIDs[message.ID] {
				combined = append(combined, message)
			}
		}
		channels[index].Messages = combined
		channels[index].FullHistoryFetchedAt = fetchedAt[channels[index].ID]
		merged++
	}
	return merged
}

// Options say how transcripts are written.
type Options struct {
	// SelfID is the owner's Discord user id, left out of "with".
	SelfID string
	// SelfName is what the owner's lines are signed with.
	SelfName string
	// People maps a Discord user id or username to a people-store id, drawn
	// as a chip.
	People map[string]string
	// Names maps a Discord user id to a name to show, for group members the
	// package names only by id.
	Names map[string]string
	// SourceEntry is the journal-store entry that describes the package.
	SourceEntry string
	Zones       residencezones.Zones
}

// Transcript is one channel's messages on one day.
type Transcript struct {
	ChannelID string
	Day       string // YYYY-MM-DD where the owner was
	Title     string
	Body      string
	Hash      string
	FirstTime time.Time
}

// Transcripts groups every channel's messages by day.
func Transcripts(channels []Channel, options Options) []Transcript {
	names := map[string]string{}
	for _, channel := range channels {
		for _, message := range channel.Messages {
			// A fetch runs newest first, so the first name seen is the latest.
			if _, known := names[message.AuthorID]; message.AuthorID != "" && message.AuthorName != "" && !known {
				names[message.AuthorID] = message.AuthorName
			}
		}
	}
	for id, name := range options.Names {
		names[id] = name
	}
	options.Names = names
	var transcripts []Transcript
	for _, channel := range channels {
		byDay := map[string][]Message{}
		var days []string
		for _, message := range channel.Messages {
			day := message.Time.In(options.Zones.At(message.Time)).Format(time.DateOnly)
			if _, seen := byDay[day]; !seen {
				days = append(days, day)
			}
			byDay[day] = append(byDay[day], message)
		}
		sort.Strings(days)
		for _, day := range days {
			messages := byDay[day]
			sort.SliceStable(messages, func(i, j int) bool { return messages[i].Time.Before(messages[j].Time) })
			body := transcriptBody(channel, messages, options)
			digest := sha256.Sum256([]byte(body))
			transcripts = append(transcripts, Transcript{
				ChannelID: channel.ID, Day: day,
				Title: fmt.Sprintf("Discord: %s, %s", channelTitle(channel, options), day),
				Body:  body, Hash: hex.EncodeToString(digest[:]), FirstTime: messages[0].Time,
			})
		}
	}
	return transcripts
}

// otherMembers are the channel's recipients other than the owner.
func otherMembers(channel Channel, options Options) []string {
	var others []string
	for _, recipient := range channel.Recipients {
		if recipient != options.SelfID {
			others = append(others, recipient)
		}
	}
	return others
}

// otherMembersAndAuthors are the channel's other members, then anyone else
// who wrote that day (someone who has since left a group), each once.
func otherMembersAndAuthors(channel Channel, messages []Message, options Options) []string {
	members := otherMembers(channel, options)
	seen := map[string]bool{options.SelfID: true}
	for _, member := range members {
		seen[member] = true
	}
	for _, message := range messages {
		if message.AuthorID != "" && !seen[message.AuthorID] {
			seen[message.AuthorID] = true
			members = append(members, message.AuthorID)
		}
	}
	return members
}

// directMessageUsername is the other side's username from index.json, or ""
// when the channel is not a DM or the index does not name it.
func directMessageUsername(channel Channel) string {
	if channel.Type != TypeDirectMessage {
		return ""
	}
	username, found := strings.CutPrefix(channel.IndexName, directMessagePrefix)
	if !found {
		return ""
	}
	return username
}

func channelTitle(channel Channel, options Options) string {
	if username := directMessageUsername(channel); username != "" {
		return username
	}
	if channel.Type == TypeGroupDirectMessage && channel.IndexName != "" && channel.IndexName != "None" {
		return "group " + channel.IndexName
	}
	var names []string
	for _, member := range otherMembers(channel, options) {
		if name := options.Names[member]; name != "" {
			names = append(names, name)
		}
	}
	if channel.Type == TypeGroupDirectMessage {
		if len(names) == 0 {
			return "group " + channel.ID
		}
		return "group with " + strings.Join(names, ", ")
	}
	if len(names) == 1 {
		return names[0]
	}
	return "DM " + channel.ID
}

// memberLabel draws one member: a chip when people-store knows them.
func memberLabel(member string, username string, options Options) string {
	name := options.Names[member]
	if username != "" {
		name = username
	}
	personID := options.People[member]
	if personID == "" && username != "" {
		personID = options.People[username]
	}
	switch {
	case personID != "" && name != "":
		return fmt.Sprintf("[%s](ref:%s) (`%s`)", transcripttext.MarkdownSafe(name), personID, member)
	case personID != "":
		return fmt.Sprintf("[%s](ref:%s) (`%s`)", member, personID, member)
	case name != "":
		return fmt.Sprintf("%s (`%s`)", transcripttext.MarkdownSafe(name), member)
	default:
		return fmt.Sprintf("`%s`", member)
	}
}

func transcriptBody(channel Channel, messages []Message, options Options) string {
	var body strings.Builder
	first := messages[0].Time.In(options.Zones.At(messages[0].Time))
	fmt.Fprintf(&body, "Discord, %s (times %s).", first.Format("Monday, January 2, 2006"), first.Format("MST"))
	if options.SourceEntry != "" {
		fmt.Fprintf(&body, " From the [Discord data package](ref:%s);", options.SourceEntry)
	}
	if channel.FullHistoryFetchedAt.IsZero() {
		fmt.Fprintf(&body, " **%s's side only**: the package holds none of the other side's messages. Channel `%s`.\n\n", options.SelfName, channel.ID)
	} else {
		fmt.Fprintf(&body, " Both sides: the whole conversation was fetched from Discord on %s. Channel `%s`.\n\n", channel.FullHistoryFetchedAt.Format(time.DateOnly), channel.ID)
	}
	username := directMessageUsername(channel)
	var members []string
	for _, member := range otherMembersAndAuthors(channel, messages, options) {
		memberUsername := ""
		if channel.Type == TypeDirectMessage {
			memberUsername = username
		}
		members = append(members, memberLabel(member, memberUsername, options))
	}
	if len(members) == 0 && username != "" {
		members = append(members, memberLabel("", username, options))
	}
	kind := "DM with"
	if channel.Type == TypeGroupDirectMessage {
		kind = "Group DM with"
	}
	fmt.Fprintf(&body, "%s: %s\n\n", kind, strings.Join(members, ", "))
	for _, message := range messages {
		clock := message.Time.In(options.Zones.At(message.Time)).Format("15:04")
		speaker := options.SelfName
		if message.AuthorID != "" && message.AuthorID != options.SelfID {
			speaker = options.Names[message.AuthorID]
		}
		if message.Event != "" {
			fmt.Fprintf(&body, "- `%s` %s **%s**: _%s_\n", message.ID, clock, transcripttext.MarkdownSafe(speaker), message.Event)
			continue
		}
		lines := strings.Split(strings.TrimSpace(message.Contents), "\n")
		fmt.Fprintf(&body, "- `%s` %s **%s**: %s\n", message.ID, clock, transcripttext.MarkdownSafe(speaker), transcripttext.MarkdownSafe(lines[0]))
		for _, more := range lines[1:] {
			fmt.Fprintf(&body, "  %s\n", transcripttext.MarkdownSafe(more))
		}
		for _, attachment := range message.Attachments {
			fmt.Fprintf(&body, "  _attachment: %s_\n", transcripttext.MarkdownSafe(attachmentName(attachment)))
		}
	}
	return body.String()
}

// attachmentName is a CDN link's file name; the link itself carries a
// signature that expires, so it is not kept. A fetched message's attachment
// is already a file name.
func attachmentName(link string) string {
	if !strings.HasPrefix(link, "http") {
		return link
	}
	parsed, err := url.Parse(link)
	if err != nil || parsed.Path == "" {
		return link
	}
	return path.Base(parsed.Path)
}

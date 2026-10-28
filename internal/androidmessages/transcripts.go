package androidmessages

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kayushkin/journal-store/internal/residencezones"
	"github.com/kayushkin/journal-store/internal/transcripttext"
)

// MaximumMembers is the most people, the user included, a conversation may
// have to be imported: direct conversations and small groups.
const MaximumMembers = 8

// Conversation is everyone but the user (Others) and what was said.
type Conversation struct {
	Others   []string
	Messages []*Message
	// Calls are the call-log rows with the one other person of a direct
	// conversation.
	Calls []*Call
}

// Key is the conversation's numbers joined by commas.
func (conversation *Conversation) Key() string { return strings.Join(conversation.Others, ",") }

// Skipped counts what Conversations left out, conversations and their
// messages.
type Skipped struct {
	// NotPhoneNumber: someone in it is a short code or an email address.
	NotPhoneNumber, NotPhoneNumberMessages int
	// TooBig: more than MaximumMembers people.
	TooBig, TooBigMessages int
	// UserNeverWrote: the user sent nothing in it (alerts, codes, mass texts).
	UserNeverWrote, UserNeverWroteMessages int
	// NoOneElse: a message with no other address at all.
	NoOneElseMessages int
	// CallsWithNoConversation: calls with a number the user has no kept
	// conversation with; BlockedCalls: calls the phone blocked.
	CallsWithNoConversation, BlockedCalls int
}

// Conversations groups the archive's messages by who is in them and keeps
// the conversations the user took part in: everyone a phone number, at most
// MaximumMembers people, and at least one message from the user. Calls join
// the direct conversation with their number.
func Conversations(archive *Archive) ([]*Conversation, Skipped) {
	var skipped Skipped
	byKey := map[string]*Conversation{}
	var order []string
	for _, message := range archive.Messages {
		if len(message.Others) == 0 {
			skipped.NoOneElseMessages++
			continue
		}
		key := strings.Join(message.Others, ",")
		conversation := byKey[key]
		if conversation == nil {
			conversation = &Conversation{Others: message.Others}
			byKey[key] = conversation
			order = append(order, key)
		}
		conversation.Messages = append(conversation.Messages, message)
	}
	var kept []*Conversation
	keptByKey := map[string]*Conversation{}
	for _, key := range order {
		conversation := byKey[key]
		count := len(conversation.Messages)
		notPhone := false
		for _, other := range conversation.Others {
			if E164(other) != other {
				notPhone = true
			}
		}
		userWrote := false
		for _, message := range conversation.Messages {
			userWrote = userWrote || message.FromSelf
		}
		switch {
		case notPhone:
			skipped.NotPhoneNumber++
			skipped.NotPhoneNumberMessages += count
		case len(conversation.Others)+1 > MaximumMembers:
			skipped.TooBig++
			skipped.TooBigMessages += count
		case !userWrote:
			skipped.UserNeverWrote++
			skipped.UserNeverWroteMessages += count
		default:
			kept = append(kept, conversation)
			keptByKey[key] = conversation
		}
	}
	for _, call := range archive.Calls {
		if call.Type == CallBlocked {
			skipped.BlockedCalls++
			continue
		}
		conversation := keptByKey[call.Number]
		if conversation == nil {
			skipped.CallsWithNoConversation++
			continue
		}
		conversation.Calls = append(conversation.Calls, call)
	}
	return kept, skipped
}

// Person is who a number belongs to in people-store.
type Person struct {
	ID          string
	DisplayName string
}

// Options say how transcripts are written.
type Options struct {
	// SelfName is how the user's own messages are signed.
	SelfName string
	Zones    residencezones.Zones
	// SourceEntry is the journal-store entry that describes the phones'
	// archive, linked from every transcript.
	SourceEntry string
	// Phones are the phones read, in order: a transcript names the ones that
	// kept its day.
	Phones []Phone
	// People are the numbers people-store knows.
	People map[string]Person
	// ContactNames are the names the phones' contact lists saved numbers as.
	ContactNames map[string][]string
	// Attachment writes one attachment as markdown: a photo-store chip, or a
	// note saying why there is none.
	Attachment func(*Message, Attachment) string
}

// Transcript is one conversation's day.
type Transcript struct {
	Conversation *Conversation
	Day          string
	// Ref is "<numbers>/<day>", the transcript's import source_ref.
	Ref       string
	Title     string
	Body      string
	Tags      []string
	Hash      string
	FirstTime time.Time
	// Messages are the day's messages, oldest first.
	Messages []*Message
	// Calls are the day's calls.
	Calls []*Call
}

// TranscriptCounts is what Transcripts wrote and left out.
type TranscriptCounts struct {
	Messages, Calls int
	// CallsOnDaysWithoutTexts are calls on a day the conversation has no
	// message: a transcript is made of texts, so they are left out.
	CallsOnDaysWithoutTexts int
	// Secrets are messages whose text was left out as a login code or
	// password.
	Secrets int
}

// Name is how a transcript names a number: the person, else the names the
// phones saved it as, else the number.
func (options Options) Name(number string) string {
	if person, known := options.People[number]; known {
		return person.DisplayName
	}
	if names := options.ContactNames[number]; len(names) > 0 {
		return strings.Join(names, " / ")
	}
	return number
}

// ConversationName is every other person's name, joined.
func (options Options) ConversationName(conversation *Conversation) string {
	names := make([]string, 0, len(conversation.Others))
	for _, other := range conversation.Others {
		names = append(names, options.Name(other))
	}
	return strings.Join(names, ", ")
}

// Transcripts writes each conversation's days, oldest first.
func Transcripts(conversations []*Conversation, options Options) ([]Transcript, TranscriptCounts) {
	var counts TranscriptCounts
	var transcripts []Transcript
	for _, conversation := range conversations {
		messagesByDay := map[string][]*Message{}
		for _, message := range conversation.Messages {
			day := message.Time.In(options.Zones.At(message.Time)).Format(time.DateOnly)
			messagesByDay[day] = append(messagesByDay[day], message)
		}
		callsByDay := map[string][]*Call{}
		for _, call := range conversation.Calls {
			day := call.Time.In(options.Zones.At(call.Time)).Format(time.DateOnly)
			if messagesByDay[day] == nil {
				counts.CallsOnDaysWithoutTexts++
				continue
			}
			callsByDay[day] = append(callsByDay[day], call)
		}
		days := make([]string, 0, len(messagesByDay))
		for day := range messagesByDay {
			days = append(days, day)
		}
		sort.Strings(days)
		for _, day := range days {
			transcript := options.transcript(conversation, day, messagesByDay[day], callsByDay[day], &counts)
			counts.Messages += len(transcript.Messages)
			counts.Calls += len(transcript.Calls)
			transcripts = append(transcripts, transcript)
		}
	}
	return transcripts, counts
}

func (options Options) phonesOf(messages []*Message, calls []*Call) []Phone {
	held := map[string]bool{}
	for _, message := range messages {
		for _, tag := range message.Phones {
			held[tag] = true
		}
	}
	for _, call := range calls {
		for _, tag := range call.Phones {
			held[tag] = true
		}
	}
	var phones []Phone
	for _, phone := range options.Phones {
		if held[phone.Tag] {
			phones = append(phones, phone)
		}
	}
	return phones
}

// joinWords is "a", "a and b", "a, b and c".
func joinWords(words []string) string {
	switch len(words) {
	case 0:
		return ""
	case 1:
		return words[0]
	}
	return strings.Join(words[:len(words)-1], ", ") + " and " + words[len(words)-1]
}

func (options Options) transcript(conversation *Conversation, day string, messages []*Message, calls []*Call, counts *TranscriptCounts) Transcript {
	sort.SliceStable(messages, func(i, j int) bool { return messages[i].Time.Before(messages[j].Time) })
	sort.SliceStable(calls, func(i, j int) bool { return calls[i].Time.Before(calls[j].Time) })
	phones := options.phonesOf(messages, calls)
	var phoneNames []string
	tags := []string{"sms"}
	for _, phone := range phones {
		phoneNames = append(phoneNames, "the "+phone.Name)
		tags = append(tags, phone.Tag)
	}
	first := messages[0].Time.In(options.Zones.At(messages[0].Time))
	var body strings.Builder
	kind := "Texts"
	if len(conversation.Others) > 1 {
		kind = "Group texts"
	}
	fmt.Fprintf(&body, "%s, %s (times %s), as kept on %s.", kind, first.Format("Monday, January 2, 2006"), first.Format("MST"), joinWords(phoneNames))
	if options.SourceEntry != "" {
		fmt.Fprintf(&body, " From the [phones' message archive](ref:%s).", options.SourceEntry)
	}
	body.WriteString("\n\n")
	var with []string
	for _, other := range conversation.Others {
		name := options.Name(other)
		label := transcripttext.MarkdownSafe(name)
		if person, known := options.People[other]; known {
			label = fmt.Sprintf("[%s](ref:%s)", transcripttext.MarkdownSafe(person.DisplayName), person.ID)
		}
		detail := "`" + other + "`"
		if saved := strings.Join(options.ContactNames[other], " / "); saved != "" && saved != name {
			detail += ", saved as " + transcripttext.MarkdownSafe(saved)
		}
		with = append(with, fmt.Sprintf("%s (%s)", label, detail))
	}
	fmt.Fprintf(&body, "With: %s\n\n", strings.Join(with, ", "))

	type line struct {
		at   time.Time
		text string
	}
	var lines []line
	for _, message := range messages {
		speaker := options.SelfName
		if !message.FromSelf {
			speaker = "someone"
			if message.Sender != "" {
				speaker = options.Name(message.Sender)
			}
		}
		var parts []string
		if message.Text != "" {
			if secret := SecretKind(message.Text); secret != "" {
				counts.Secrets++
				parts = append(parts, "_"+secret+", left out_")
			} else {
				pieces := strings.Split(strings.TrimSpace(message.Text), "\n")
				parts = append(parts, transcripttext.MarkdownSafe(strings.Join(pieces, " / ")))
			}
		}
		for _, attachment := range message.Attachments {
			parts = append(parts, options.Attachment(message, attachment))
		}
		if len(parts) == 0 {
			parts = append(parts, "_an empty message_")
		}
		lines = append(lines, line{message.Time, fmt.Sprintf("**%s**: %s", transcripttext.MarkdownSafe(speaker), strings.Join(parts, " "))})
	}
	for _, call := range calls {
		lines = append(lines, line{call.Time, "_" + describeCall(call, options.Name(call.Number)) + "_"})
	}
	sort.SliceStable(lines, func(i, j int) bool { return lines[i].at.Before(lines[j].at) })
	for _, entry := range lines {
		fmt.Fprintf(&body, "- %s %s\n", entry.at.In(options.Zones.At(entry.at)).Format("15:04"), entry.text)
	}
	title := fmt.Sprintf("Texts: %s, %s", options.ConversationName(conversation), day)
	digest := sha256.Sum256([]byte(title + "\x00" + strings.Join(tags, ",") + "\x00" + body.String()))
	return Transcript{
		Conversation: conversation, Day: day, Ref: conversation.Key() + "/" + day,
		Title: title, Body: body.String(), Tags: tags, Hash: hex.EncodeToString(digest[:]),
		FirstTime: messages[0].Time, Messages: messages, Calls: calls,
	}
}

func minutes(seconds int) string {
	if seconds < 60 {
		return fmt.Sprintf("%d s", seconds)
	}
	return fmt.Sprintf("%d min", (seconds+59)/60)
}

func describeCall(call *Call, name string) string {
	name = transcripttext.MarkdownSafe(name)
	switch call.Type {
	case CallIncoming:
		return fmt.Sprintf("call from %s, %s", name, minutes(call.DurationSeconds))
	case CallOutgoing:
		if call.DurationSeconds == 0 {
			return fmt.Sprintf("call to %s, not answered", name)
		}
		return fmt.Sprintf("call to %s, %s", name, minutes(call.DurationSeconds))
	case CallMissed:
		return "missed call from " + name
	case CallVoicemail:
		return "voicemail from " + name
	case CallRejected:
		return "declined call from " + name
	}
	return fmt.Sprintf("call with %s (call log type %d)", name, call.Type)
}

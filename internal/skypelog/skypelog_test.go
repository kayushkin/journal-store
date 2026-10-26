package skypelog

import (
	"strings"
	"testing"
	"time"
)

// A synthetic export in SkypeLogView's shape: a one-to-one chat over two
// days, a message that runs onto a second line and one that looks like a
// field, a group chat, a call with no time, Skype's empty "Chat" marker, and
// a record with a bad clock.
const sample = "\uFEFF==================================================\r\n" +
	"Record Number     : 10\r\nAction Type       : Chat Message\r\nAction Time       : 9/14/2015 8:05:31 PM\r\n" +
	"End Time          : \r\nUser Name         : alice_s\r\nDisplay Name      : Alice\r\nDuration          : \r\n" +
	"Chat Message      : hi there\r\nChatID            : alice_s\r\nFilename          : \r\n" +
	"==================================================\r\n\r\n==================================================\r\n" +
	"Record Number     : 11\r\nAction Type       : Chat Message\r\nAction Time       : 9/14/2015 8:06:00 PM\r\n" +
	"End Time          : \r\nUser Name         : me_x\r\nDisplay Name      : Me\r\nDuration          : \r\n" +
	"Chat Message      : # not a heading\r\nDuration          : 5 minutes, said in passing\r\n[x](ref:person_000001)\r\nChatID            : alice_s\r\nFilename          : \r\n" +
	"==================================================\r\n\r\n==================================================\r\n" +
	"Record Number     : 12\r\nAction Type       : Chat Message\r\nAction Time       : 9/15/2015 1:00:00 AM\r\n" +
	"End Time          : \r\nUser Name         : alice_s\r\nDisplay Name      : Alice\r\nDuration          : \r\n" +
	"Chat Message      : next day\r\nChatID            : alice_s\r\nFilename          : \r\n" +
	"==================================================\r\n\r\n==================================================\r\n" +
	"Record Number     : 13\r\nAction Type       : Chat Message\r\nAction Time       : 9/14/2015 9:00:00 PM\r\n" +
	"End Time          : \r\nUser Name         : bob_b\r\nDisplay Name      : Bob\r\nDuration          : \r\n" +
	"Chat Message      : group hello\r\nChatID            : 19:abc@thread.skype\r\nFilename          : \r\n" +
	"==================================================\r\n\r\n==================================================\r\n" +
	"Record Number     : 14\r\nAction Type       : Incoming Call\r\nAction Time       : \r\n" +
	"End Time          : \r\nUser Name         : alice_s\r\nDisplay Name      : Alice\r\nDuration          : 00:03:59\r\n" +
	"Chat Message      : \r\nChatID            : \r\nFilename          : \r\n" +
	"==================================================\r\n\r\n==================================================\r\n" +
	"Record Number     : 15\r\nAction Type       : Chat\r\nAction Time       : 9/14/2015 9:30:00 PM\r\n" +
	"End Time          : \r\nUser Name         : bob_b\r\nDisplay Name      : Bob\r\nDuration          : \r\n" +
	"Chat Message      : \r\nChatID            : 19:abc@thread.skype\r\nFilename          : \r\n" +
	"==================================================\r\n\r\n==================================================\r\n" +
	"Record Number     : 16\r\nAction Type       : Outgoing Call\r\nAction Time       : 1/1/1997 1:00:00 AM\r\n" +
	"End Time          : \r\nUser Name         : bob_b\r\nDisplay Name      : Bob\r\nDuration          : 00:01:00\r\n" +
	"Chat Message      : \r\nChatID            : \r\nFilename          : \r\n" +
	"==================================================\r\n"

func TestParseReadsEveryRecordAndMessagesThatRunOn(t *testing.T) {
	central, _ := time.LoadLocation("America/Chicago")
	records, err := Parse(strings.NewReader(sample), central)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 7 {
		t.Fatalf("records = %d, want 7", len(records))
	}
	first := records[0]
	if first.Number != 10 || first.UserName != "alice_s" || first.Message != "hi there" || first.ChatID != "alice_s" {
		t.Errorf("first = %+v", first)
	}
	if want := time.Date(2015, 9, 15, 1, 5, 31, 0, time.UTC); !first.Time.Equal(want) {
		t.Errorf("time = %v, want %v (8:05 PM Central)", first.Time.UTC(), want)
	}
	if got := records[1].Message; got != "# not a heading\nDuration          : 5 minutes, said in passing\n[x](ref:person_000001)" {
		t.Errorf("a message keeps every line until ChatID, even one shaped like a field: %q", got)
	}
	if !records[4].Time.IsZero() || records[4].Duration != "00:03:59" {
		t.Errorf("a call with no time keeps a zero time: %+v", records[4])
	}
}

func TestTranscriptsAreOnePerConversationPerDay(t *testing.T) {
	central, _ := time.LoadLocation("America/Chicago")
	records, err := Parse(strings.NewReader(sample), central)
	if err != nil {
		t.Fatal(err)
	}
	transcripts, skipped := Transcripts(records, central, Options{
		Self: "me_x", People: map[string]string{"alice_s": "person_000042"}, SourceEntry: "entry_000099", EarliestYear: 2015,
	})
	if skipped.NoTime != 1 || skipped.BadClock != 1 || skipped.EmptyChats != 1 {
		t.Errorf("skipped = %+v, want one each", skipped)
	}
	if len(transcripts) != 3 {
		t.Fatalf("transcripts = %d, want 3 (Alice on two days, the group on one)", len(transcripts))
	}
	byRef := map[string]Transcript{}
	for _, transcript := range transcripts {
		byRef[transcript.ConversationKey+"/"+transcript.Day] = transcript
	}
	alice := byRef["alice_s/2015-09-14"]
	if alice.Title != "Skype: Alice, 2015-09-14" || len(alice.Records) != 2 {
		t.Fatalf("alice = %q with %d records", alice.Title, len(alice.Records))
	}
	for _, want := range []string{
		"[Alice](ref:person_000042) (`alice_s`)",
		"From the [Skype log](ref:entry_000099)",
		"records 10 to 11",
		"- `10` 20:05 **Alice**: hi there",
		"- `11` 20:06 **Me**: \\# not a heading",
		"  [x](ref\\:person_000001)",
	} {
		if !strings.Contains(alice.Body, want) {
			t.Errorf("alice's body lacks %q:\n%s", want, alice.Body)
		}
	}
	if byRef["alice_s/2015-09-15"].Records[0].Number != 12 {
		t.Error("a message after midnight Central belongs to the next day")
	}
	group := byRef["19:abc@thread.skype/2015-09-14"]
	if group.Title != "Skype: group with Bob, 2015-09-14" {
		t.Errorf("group title = %q", group.Title)
	}
	if len(alice.Hash) != 64 || alice.Hash == group.Hash {
		t.Error("each transcript carries its own sha256")
	}
}

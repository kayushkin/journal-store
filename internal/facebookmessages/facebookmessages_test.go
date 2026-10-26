package facebookmessages

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kayushkin/journal-store/internal/residencezones"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSmallThreadsBecomeRepairedTranscriptsAndBigOnesAreLeftOut(t *testing.T) {
	dir := t.TempDir()
	// "Zoë" and "👍" as the export writes them: each UTF-8 byte as a \u00XX escape.
	write(t, filepath.Join(dir, "inbox", "111", "message_1.json"), `{"title":"ZoÃ«","participants":[{"name":"ZoÃ«"},{"name":"Me Self"}],
	"messages":[
	 {"sender_name":"ZoÃ«","timestamp_ms":1590000060000,"content":"# hey ð\u009f\u0091\u008d","reactions":[{"reaction":"ð\u009f\u0091\u008d","actor":"Me Self"}]},
	 {"sender_name":"Me Self","timestamp_ms":1590000000000,"photos":[{"uri":"your_facebook_activity/messages/inbox/111/photos/cat.jpg"}]},
	 {"sender_name":"ZoÃ«","timestamp_ms":1590000120000,"call_duration":61}]}`)
	big := `{"title":"Huge","participants":[` + strings.Repeat(`{"name":"x"},`, 8) + `{"name":"Me Self"}],"messages":[{"sender_name":"x","timestamp_ms":1590000000000,"content":"hi"}]}`
	write(t, filepath.Join(dir, "inbox", "222", "message_1.json"), big)
	threads, skipped, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(threads) != 1 || skipped.TooManyParticipants != 1 || skipped.Messages != 1 {
		t.Fatalf("threads %d, skipped %+v", len(threads), skipped)
	}
	zones, _ := residencezones.Parse("UTC")
	transcripts := Transcripts(threads, Options{SelfName: "Me Self", Zones: zones, People: map[string]string{"Zoë": "person_000007"}})
	if len(transcripts) != 1 || transcripts[0].Title != "Messenger: Zoë, 2020-05-20" {
		t.Fatalf("transcripts: %+v", transcripts)
	}
	want := "With: [Zoë](ref:person_000007)\n\n" +
		"- 18:40 **Me Self**: _photo: cat.jpg_\n" +
		"- 18:41 **Zoë**: \\# hey 👍\n  _Me Self reacted 👍_\n" +
		"- 18:42 **Zoë**: _call, 2 min_\n"
	if !strings.Contains(transcripts[0].Body, want) {
		t.Fatalf("body:\n%s\nwant:\n%s", transcripts[0].Body, want)
	}
}

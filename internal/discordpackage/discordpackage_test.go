package discordpackage

import (
	"github.com/kayushkin/journal-store/internal/residencezones"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writePackage lays out a synthetic Messages folder: a DM, a group DM and a
// server channel.
func writePackage(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"index.json":         `{"100":"Direct Message with alice_s","200":"None","300":"general in Some Server"}`,
		"c100/channel.json":  `{"id":"100","type":"DM","recipients":["9","1"]}`,
		"c100/messages.json": `[{"ID":12,"Timestamp":"2021-07-11 03:00:00","Contents":"after midnight east","Attachments":""},{"ID":11,"Timestamp":"2021-07-10 15:00:00","Contents":"# hi\n[x](ref:person_000001)","Attachments":"https://cdn.discordapp.com/attachments/1/2/cat.png?ex=0&hm=abc"}]`,
		"c200/channel.json":  `{"id":"200","type":"GROUP_DM","recipients":["9","2","3"]}`,
		"c200/messages.json": `[{"ID":21,"Timestamp":"2021-07-14 18:00:00","Contents":"hello all","Attachments":""}]`,
		"c300/channel.json":  `{"id":"300","type":"GUILD_TEXT"}`,
		"c300/messages.json": `[{"ID":31,"Timestamp":"2021-07-14 18:00:00","Contents":"server talk","Attachments":""}]`,
	}
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestTranscriptsKeepDirectMessagesAndFileByWhereTheOwnerLived(t *testing.T) {
	channels, leftOut, err := Read(writePackage(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(channels) != 2 || leftOut != 1 {
		t.Fatalf("read %d channels, left out %d; want 2 and 1", len(channels), leftOut)
	}
	zones, err := residencezones.Parse("Europe/Lisbon,2021-07-11=America/Chicago")
	if err != nil {
		t.Fatal(err)
	}
	transcripts := Transcripts(channels, Options{
		SelfID: "9", SelfName: "Me", Zones: zones, SourceEntry: "entry_000009",
		People: map[string]string{"alice_s": "person_000005", "2": "person_000006"},
		Names:  map[string]string{"3": "Bob"},
	})
	var titles []string
	for _, transcript := range transcripts {
		titles = append(titles, transcript.ChannelID+"/"+transcript.Day+" "+transcript.Title)
	}
	// 2021-07-11 03:00 UTC is after the move took effect, so it is filed on
	// the 10th Central, with the message from 15:00 UTC on the 10th.
	want := []string{
		"100/2021-07-10 Discord: alice_s, 2021-07-10",
		"200/2021-07-14 Discord: group with Bob, 2021-07-14",
	}
	if strings.Join(titles, "\n") != strings.Join(want, "\n") {
		t.Fatalf("titles:\n%s\nwant:\n%s", strings.Join(titles, "\n"), strings.Join(want, "\n"))
	}
	direct := transcripts[0].Body
	for _, part := range []string{
		"[alice_s](ref:person_000005) (`1`)",
		"(ref:entry_000009)",
		"- `11` 16:00 **Me**: \\# hi\n  [x](ref\\:person_000001)\n  _attachment: cat.png_\n- `12` 22:00 **Me**: after midnight east",
		"Me's side only",
	} {
		if !strings.Contains(direct, part) {
			t.Errorf("DM body lacks %q:\n%s", part, direct)
		}
	}
	if strings.Contains(direct, "hm=abc") {
		t.Errorf("DM body keeps the signed link:\n%s", direct)
	}
	if group := transcripts[1].Body; !strings.Contains(group, "Group DM with: [2](ref:person_000006) (`2`), Bob (`3`)") {
		t.Errorf("group body:\n%s", group)
	}
}

func TestFullHistoryGivesBothSidesAndKeepsMessagesTheFetchLacks(t *testing.T) {
	channels, _, err := Read(writePackage(t))
	if err != nil {
		t.Fatal(err)
	}
	folder := t.TempDir()
	fetched := `{"id":"13","type":0,"timestamp":"2021-07-10T16:00:00+00:00","content":"reply from alice","author":{"id":"1","username":"alice_s","global_name":"Alice"},"attachments":[{"filename":"dog.jpg"}]}
{"id":"11","type":0,"timestamp":"2021-07-10T15:00:00.123+00:00","content":"# hi","author":{"id":"9","username":"me"},"attachments":[]}
{"id":"14","type":3,"timestamp":"2021-07-10T17:00:00+00:00","content":"","author":{"id":"1","username":"alice_s"},"attachments":[]}
`
	if err := os.WriteFile(filepath.Join(folder, "100.jsonl"), []byte(fetched), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, "100.done"), []byte("2026-10-26T19:00:00Z\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// An unfinished fetch is not used.
	if err := os.WriteFile(filepath.Join(folder, "200.jsonl"), []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	histories, fetchedAt, err := ReadFullHistory(folder)
	if err != nil {
		t.Fatal(err)
	}
	if merged := MergeFullHistory(channels, histories, fetchedAt); merged != 1 {
		t.Fatalf("merged %d channels, want 1", merged)
	}
	zones, _ := residencezones.Parse("UTC")
	transcripts := Transcripts(channels, Options{SelfID: "9", SelfName: "Me", Zones: zones})
	body := transcripts[0].Body
	for _, part := range []string{
		"Both sides: the whole conversation was fetched from Discord on 2026-10-26.",
		"- `11` 15:00 **Me**: \\# hi\n- `13` 16:00 **Alice**: reply from alice\n  _attachment: dog.jpg_\n- `14` 17:00 **Alice**: _call_\n",
	} {
		if !strings.Contains(body, part) {
			t.Errorf("body lacks %q:\n%s", part, body)
		}
	}
	// Message 12, in the package but not the fetch, is kept on its own day.
	if len(transcripts) < 2 || !strings.Contains(transcripts[1].Body, "after midnight east") {
		t.Errorf("the package-only message was lost: %+v", transcripts)
	}
	if strings.Contains(transcripts[2].Body, "Both sides") {
		t.Errorf("the group DM, never fetched, claims both sides:\n%s", transcripts[2].Body)
	}
}

// Command journal-timeline-discord-events puts the Discord events the
// operator signed up for on journal timelines: each on the timeline kept for
// its server, and each again on the timeline of every other person on the
// roster who has one.
//
// discord-signup-store holds the events and their rosters, people-store says
// which person a Discord account belongs to, and this command writes timeline
// items that cite the event by its ref (discord_event_000126). A roster
// records who signed up, not who turned up, and the items say so.
//
// A repeating event is one roster that moves to its next date; each date that
// has passed is read back from the event's history: the start it moved away
// from, and who was on the roster just before it moved.
//
// Nothing is written twice. A timeline that already has an item citing the
// event, for that date, gets no other, and a deleted item counts: an item the
// operator removed is not put back. So an item written by hand from chats and
// photos stops this command from adding its own once it cites the event.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	journalstore "github.com/kayushkin/journal-store"
)

var client = &http.Client{Timeout: time.Minute}

// sameOccurrenceWindow is how close an item's time must be to a repeating
// event's date to count as that date: the dates are a week or more apart.
const sameOccurrenceWindow = 36 * 3600

// eventListLimit and historyLimit are the most discord-signup-store lists in
// one call of GET /api/events and GET /api/events/{id}/history.
const (
	eventListLimit = 500
	historyLimit   = 1000
)

type discordEvent struct {
	ID             int64  `json:"id"`
	Ref            string `json:"ref"`
	GuildID        string `json:"guild_id"`
	Name           string `json:"name"`
	Status         string `json:"status"`
	StartsAt       int64  `json:"starts_at"`
	EndsAt         int64  `json:"ends_at"`
	Location       string `json:"location"`
	RecurrenceRule string `json:"recurrence_rule"`
}

type signup struct {
	DiscordUserID string `json:"discord_user_id"`
	DisplayName   string `json:"display_name"`
	State         string `json:"state"`
	SignedUpAt    int64  `json:"signed_up_at"`
}

type signupUpdate struct {
	ID            int64  `json:"id"`
	DiscordUserID string `json:"discord_user_id"`
	ToState       string `json:"to_state"`
	At            int64  `json:"at"`
	DisplayName   string `json:"display_name"`
}

type eventUpdate struct {
	ID        int64  `json:"id"`
	Field     string `json:"field"`
	FromValue string `json:"from_value"`
	Actor     string `json:"actor"`
	At        int64  `json:"at"`
}

// attendee is one person on a roster for one date.
type attendee struct {
	DiscordUserID string
	DisplayName   string
}

// occurrence is one date of an event and who was on its roster for it.
type occurrence struct {
	Event     discordEvent
	StartsAt  int64
	EndsAt    int64
	Attending []attendee
}

type timelineItem struct {
	ID         string   `json:"id"`
	TimelineID string   `json:"timeline_id"`
	OccurredAt int64    `json:"occurred_at"`
	Sources    []string `json:"sources"`
}

type person struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
}

func getJSON(address string, into any) (found bool, err error) {
	response, err := client.Get(address)
	if err != nil {
		return false, err
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if response.StatusCode != http.StatusOK {
		return false, fmt.Errorf("GET %s: %s: %s", address, response.Status, strings.TrimSpace(string(body)))
	}
	if err := json.Unmarshal(body, into); err != nil {
		return false, fmt.Errorf("GET %s: %w", address, err)
	}
	return true, nil
}

func mustGetJSON(address string, into any) error {
	found, err := getJSON(address, into)
	if err == nil && !found {
		err = fmt.Errorf("GET %s: 404", address)
	}
	return err
}

func postJSON(address string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	response, err := client.Post(address, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer response.Body.Close()
	answer, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusCreated {
		return fmt.Errorf("POST %s: %s: %s", address, response.Status, strings.TrimSpace(string(answer)))
	}
	return nil
}

// occurrencesOf lists the dates of an event that have passed, each with who
// was on the roster for it.
func occurrencesOf(event discordEvent, roster []signup, history []signupUpdate, updates []eventUpdate, now int64) []occurrence {
	if event.RecurrenceRule == "" {
		end := event.EndsAt
		if end == 0 {
			end = event.StartsAt
		}
		if event.StartsAt == 0 || end > now {
			return nil
		}
		single := occurrence{Event: event, StartsAt: event.StartsAt, EndsAt: event.EndsAt}
		for _, signup := range roster {
			if signup.State == "attending" {
				single.Attending = append(single.Attending, attendee{signup.DiscordUserID, signup.DisplayName})
			}
		}
		return []occurrence{single}
	}
	// A repeating event: each move of starts_at by the recurrence is one date
	// that ran. Its end moved in the same instant.
	endsMovedAt := map[int64]int64{}
	for _, update := range updates {
		if update.Field == "ends_at" && update.Actor == "recurrence" {
			endsAt, _ := strconv.ParseInt(update.FromValue, 10, 64)
			endsMovedAt[update.At] = endsAt
		}
	}
	sort.Slice(history, func(i, j int) bool {
		if history[i].At != history[j].At {
			return history[i].At < history[j].At
		}
		return history[i].ID < history[j].ID
	})
	var occurrences []occurrence
	for _, update := range updates {
		if update.Field != "starts_at" || update.Actor != "recurrence" {
			continue
		}
		startsAt, err := strconv.ParseInt(update.FromValue, 10, 64)
		if err != nil || startsAt == 0 {
			continue
		}
		occurrences = append(occurrences, occurrence{
			Event: event, StartsAt: startsAt, EndsAt: endsMovedAt[update.At],
			Attending: rosterBefore(history, update.At),
		})
	}
	return occurrences
}

// rosterBefore replays the roster's history up to, not including, moment:
// the move to the next date withdraws everyone in that same instant.
func rosterBefore(history []signupUpdate, moment int64) []attendee {
	state := map[string]signupUpdate{}
	var order []string
	for _, update := range history {
		if update.At >= moment {
			break
		}
		if _, seen := state[update.DiscordUserID]; !seen {
			order = append(order, update.DiscordUserID)
		}
		state[update.DiscordUserID] = update
	}
	var attending []attendee
	for _, userID := range order {
		if last := state[userID]; last.ToState == "attending" {
			attending = append(attending, attendee{userID, last.DisplayName})
		}
	}
	return attending
}

// hasItemFor says whether a timeline's items, deleted ones included, already
// hold this date of this event.
func hasItemFor(items []timelineItem, occurrence occurrence) bool {
	for _, item := range items {
		for _, source := range item.Sources {
			if source != occurrence.Event.Ref {
				continue
			}
			if occurrence.Event.RecurrenceRule == "" {
				return true
			}
			distance := item.OccurredAt - occurrence.StartsAt
			if distance < 0 {
				distance = -distance
			}
			if distance <= sameOccurrenceWindow {
				return true
			}
		}
	}
	return false
}

// markdownText keeps a name from closing the link it sits in.
func markdownText(text string) string {
	return strings.NewReplacer("[", "(", "]", ")").Replace(text)
}

func itemTimes(occurrence occurrence) (int64, int64) {
	endsAt := occurrence.EndsAt
	if endsAt < occurrence.StartsAt {
		endsAt = 0
	}
	return occurrence.StartsAt, endsAt
}

type importer struct {
	journalURL, signupURL, peopleURL string
	operatorDiscordID                string
	matrixHomeserver                 string
	dryRun                           bool

	timelineItems   map[string][]timelineItem
	personByDiscord map[string]*person
	personTimelines map[string][]string
	written         int
}

func (importer *importer) itemsOf(timelineID string) ([]timelineItem, error) {
	if items, ok := importer.timelineItems[timelineID]; ok {
		return items, nil
	}
	var items []timelineItem
	if err := mustGetJSON(importer.journalURL+"/timelines/"+timelineID+"/items?include_deleted=true", &items); err != nil {
		return nil, err
	}
	importer.timelineItems[timelineID] = items
	return items, nil
}

// personOf finds the people-store person a Discord account belongs to, by the
// account itself or by the Matrix handle multichat's Discord bridge gives it.
func (importer *importer) personOf(discordUserID string) (*person, error) {
	if found, ok := importer.personByDiscord[discordUserID]; ok {
		return found, nil
	}
	handles := [][2]string{{"discord", discordUserID}}
	if importer.matrixHomeserver != "" {
		handles = append(handles, [2]string{"matrix_user", "@discord_" + discordUserID + ":" + importer.matrixHomeserver})
	}
	var found *person
	for _, handle := range handles {
		var answer struct {
			Person person `json:"person"`
		}
		ok, err := getJSON(importer.peopleURL+"/identities/resolve?kind="+handle[0]+"&value="+url.QueryEscape(handle[1]), &answer)
		if err != nil {
			return nil, err
		}
		if ok {
			found = &answer.Person
			break
		}
	}
	importer.personByDiscord[discordUserID] = found
	return found, nil
}

func (importer *importer) timelinesAbout(personID string) ([]string, error) {
	if ids, ok := importer.personTimelines[personID]; ok {
		return ids, nil
	}
	var entries []struct {
		ID string `json:"id"`
	}
	if err := mustGetJSON(importer.journalURL+"/entries?kind=timeline&about_ref="+url.QueryEscape(personID), &entries); err != nil {
		return nil, err
	}
	var ids []string
	for _, entry := range entries {
		ids = append(ids, entry.ID)
	}
	importer.personTimelines[personID] = ids
	return ids, nil
}

func (importer *importer) write(timelineID string, item journalstore.TimelineItemInput) error {
	importer.written++
	if importer.dryRun {
		log.Printf("would add to %s: %s %s %q", timelineID, time.Unix(item.OccurredAt, 0).UTC().Format(time.RFC3339), item.Sources[0], item.Title)
		return nil
	}
	if err := postJSON(importer.journalURL+"/timelines/"+timelineID+"/items", journalstore.TimelineItemsRequest{Items: []journalstore.TimelineItemInput{item}}); err != nil {
		return err
	}
	log.Printf("added to %s: %s %q", timelineID, item.Sources[0], item.Title)
	// Read again before the next date, so a second date of a repeating event
	// sees this one.
	delete(importer.timelineItems, timelineID)
	return nil
}

func (importer *importer) placeOccurrence(occurrence occurrence, groupTimelineID string) error {
	operatorWent := false
	var others []string
	type personOnRoster struct {
		person *person
		name   string
	}
	var people []personOnRoster
	for _, attendee := range occurrence.Attending {
		if attendee.DiscordUserID == importer.operatorDiscordID {
			operatorWent = true
			continue
		}
		found, err := importer.personOf(attendee.DiscordUserID)
		if err != nil {
			return err
		}
		if found != nil {
			others = append(others, fmt.Sprintf("[%s](ref:%s)", markdownText(found.DisplayName), found.ID))
		} else {
			others = append(others, markdownText(attendee.DisplayName))
		}
		people = append(people, personOnRoster{found, attendee.DisplayName})
	}
	if !operatorWent {
		return nil
	}
	event := occurrence.Event
	startsAt, endsAt := itemTimes(occurrence)
	where := ""
	if event.Location != "" {
		where = " at " + markdownText(event.Location)
	}
	withWhom := "No one else was on the roster."
	if len(others) > 0 {
		withWhom = fmt.Sprintf("On the roster with him (%d): %s.", len(others), strings.Join(others, ", "))
	}
	items, err := importer.itemsOf(groupTimelineID)
	if err != nil {
		return err
	}
	if !hasItemFor(items, occurrence) {
		body := fmt.Sprintf("Signed up for [%s](ref:%s)%s. %s The roster records who signed up, not who turned up.",
			markdownText(event.Name), event.Ref, where, withWhom)
		if err := importer.write(groupTimelineID, journalstore.TimelineItemInput{
			OccurredAt: startsAt, EndsAt: endsAt, Precision: "minute",
			Title: event.Name, Body: body, Sources: []string{event.Ref},
		}); err != nil {
			return err
		}
	}
	for _, onRoster := range people {
		if onRoster.person == nil {
			continue
		}
		timelineIDs, err := importer.timelinesAbout(onRoster.person.ID)
		if err != nil {
			return err
		}
		for _, timelineID := range timelineIDs {
			items, err := importer.itemsOf(timelineID)
			if err != nil {
				return err
			}
			if hasItemFor(items, occurrence) {
				continue
			}
			body := fmt.Sprintf("Both signed up for [%s](ref:%s)%s, with %d others ([the group's timeline](ref:%s)). The roster records who signed up, not who turned up.",
				markdownText(event.Name), event.Ref, where, len(occurrence.Attending)-2, groupTimelineID)
			if err := importer.write(timelineID, journalstore.TimelineItemInput{
				OccurredAt: startsAt, EndsAt: endsAt, Precision: "minute",
				Title: event.Name, Body: body, Sources: []string{event.Ref},
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

type guildTimelines map[string]string

func (g guildTimelines) String() string { return fmt.Sprint(map[string]string(g)) }
func (g guildTimelines) Set(value string) error {
	guildID, timelineID, ok := strings.Cut(value, "=")
	if !ok || guildID == "" || timelineID == "" {
		return fmt.Errorf("want <guild id>=<timeline entry id>, got %q", value)
	}
	g[guildID] = timelineID
	return nil
}

func main() {
	journalURL := flag.String("journal-store-url", "http://"+journalstore.DefaultListenAddress, "journal-store's base URL")
	signupURL := flag.String("discord-signup-store-url", "", "discord-signup-store's base URL (required)")
	peopleURL := flag.String("people-store-url", "", "people-store's base URL (required)")
	operatorDiscordID := flag.String("operator-discord-id", "", "the Discord user id whose events these are (required)")
	matrixHomeserver := flag.String("matrix-homeserver", "", "also find a person by the Matrix handle @discord_<id>:<this> that multichat's bridge gives a Discord account; empty: only by kind discord")
	guilds := guildTimelines{}
	flag.Var(guilds, "guild-timeline", "<guild id>=<timeline entry id>: the timeline for that server's events; repeat for each server. Events in other servers are left alone")
	dryRun := flag.Bool("dry-run", false, "say what would be added and write nothing")
	flag.Parse()
	if *signupURL == "" || *peopleURL == "" || *operatorDiscordID == "" || len(guilds) == 0 {
		flag.Usage()
		os.Exit(2)
	}
	importer := &importer{
		journalURL: *journalURL, signupURL: *signupURL, peopleURL: *peopleURL,
		operatorDiscordID: *operatorDiscordID, matrixHomeserver: *matrixHomeserver, dryRun: *dryRun,
		timelineItems: map[string][]timelineItem{}, personByDiscord: map[string]*person{}, personTimelines: map[string][]string{},
	}
	if err := importer.run(guilds, time.Now().Unix()); err != nil {
		log.Fatal(err)
	}
}

func (importer *importer) run(guilds guildTimelines, now int64) error {
	var listed struct {
		Events []discordEvent `json:"events"`
	}
	if err := mustGetJSON(fmt.Sprintf("%s/api/events?limit=%d", importer.signupURL, eventListLimit), &listed); err != nil {
		return err
	}
	if len(listed.Events) >= eventListLimit {
		return fmt.Errorf("discord-signup-store listed %d events, its most in one call; some would be missed", len(listed.Events))
	}
	events := 0
	for _, event := range listed.Events {
		groupTimelineID, ok := guilds[event.GuildID]
		if !ok || event.Status == "cancelled" {
			continue
		}
		if event.Ref == "" {
			return fmt.Errorf("event %d came without a ref; discord-signup-store is older than refs", event.ID)
		}
		var roster struct {
			Signups []signup `json:"signups"`
		}
		var history struct {
			SignupUpdates []signupUpdate `json:"signup_updates"`
		}
		var updates struct {
			EventUpdates []eventUpdate `json:"event_updates"`
		}
		base := fmt.Sprintf("%s/api/events/%d", importer.signupURL, event.ID)
		if err := mustGetJSON(base+"/signups", &roster); err != nil {
			return err
		}
		if event.RecurrenceRule != "" {
			if err := mustGetJSON(fmt.Sprintf("%s/history?limit=%d", base, historyLimit), &history); err != nil {
				return err
			}
			if len(history.SignupUpdates) >= historyLimit {
				return fmt.Errorf("%s: its roster history reached %d rows, the most in one call; later dates would be read wrong", event.Ref, historyLimit)
			}
			if err := mustGetJSON(base+"/updates", &updates); err != nil {
				return err
			}
		}
		for _, occurrence := range occurrencesOf(event, roster.Signups, history.SignupUpdates, updates.EventUpdates, now) {
			events++
			if err := importer.placeOccurrence(occurrence, groupTimelineID); err != nil {
				return fmt.Errorf("%s: %w", event.Ref, err)
			}
		}
	}
	verb := "added"
	if importer.dryRun {
		verb = "would add"
	}
	log.Printf("%d past event dates read; %s %d timeline items", events, verb, importer.written)
	return nil
}

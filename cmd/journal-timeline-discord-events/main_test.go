package main

import "testing"

func TestOneDateEventIsReadFromItsRoster(t *testing.T) {
	event := discordEvent{ID: 1, Ref: "discord_event_000001", StartsAt: 1000, EndsAt: 2000}
	roster := []signup{
		{DiscordUserID: "a", DisplayName: "Ann", State: "attending"},
		{DiscordUserID: "b", DisplayName: "Bo", State: "withdrawn"},
		{DiscordUserID: "c", DisplayName: "Cy", State: "waitlisted"},
	}
	if got := occurrencesOf(event, roster, nil, nil, 1500); got != nil {
		t.Fatalf("an event still running was read: %+v", got)
	}
	got := occurrencesOf(event, roster, nil, nil, 3000)
	if len(got) != 1 || len(got[0].Attending) != 1 || got[0].Attending[0].DiscordUserID != "a" {
		t.Fatalf("got %+v, want one date with only Ann", got)
	}
}

func TestRepeatingEventDatesComeFromItsHistory(t *testing.T) {
	event := discordEvent{ID: 2, Ref: "discord_event_000002", StartsAt: 900000, RecurrenceRule: "FREQ=WEEKLY"}
	updates := []eventUpdate{
		{ID: 1, Field: "starts_at", FromValue: "100000", Actor: "recurrence", At: 110000},
		{ID: 2, Field: "ends_at", FromValue: "105000", Actor: "recurrence", At: 110000},
		{ID: 3, Field: "name", FromValue: "old", Actor: "discord:x", At: 120000},
		{ID: 4, Field: "starts_at", FromValue: "700000", Actor: "recurrence", At: 710000},
	}
	history := []signupUpdate{
		{ID: 1, DiscordUserID: "a", ToState: "attending", At: 50000},
		{ID: 2, DiscordUserID: "b", ToState: "attending", At: 60000},
		{ID: 3, DiscordUserID: "b", ToState: "withdrawn", At: 60004},
		// the move to the next date withdraws everyone in the same instant
		{ID: 4, DiscordUserID: "a", ToState: "withdrawn", At: 110000},
		{ID: 5, DiscordUserID: "b", ToState: "attending", At: 600000},
		{ID: 6, DiscordUserID: "b", ToState: "withdrawn", At: 710000},
	}
	got := occurrencesOf(event, nil, history, updates, 2000000)
	if len(got) != 2 {
		t.Fatalf("got %d dates, want 2: %+v", len(got), got)
	}
	if got[0].StartsAt != 100000 || got[0].EndsAt != 105000 || len(got[0].Attending) != 1 || got[0].Attending[0].DiscordUserID != "a" {
		t.Errorf("first date = %+v, want Ann alone at 100000-105000", got[0])
	}
	if got[1].StartsAt != 700000 || len(got[1].Attending) != 1 || got[1].Attending[0].DiscordUserID != "b" {
		t.Errorf("second date = %+v, want Bo alone at 700000", got[1])
	}
}

func TestAnItemCitingTheEventHoldsItsDate(t *testing.T) {
	once := occurrence{Event: discordEvent{Ref: "discord_event_000001"}, StartsAt: 1000}
	weekly := occurrence{Event: discordEvent{Ref: "discord_event_000002", RecurrenceRule: "FREQ=WEEKLY"}, StartsAt: 1000000}
	items := []timelineItem{
		{ID: "x", OccurredAt: 5000, Sources: []string{"message_000001", "discord_event_000001"}},
		{ID: "y", OccurredAt: 1000000 - 7*86400, Sources: []string{"discord_event_000002"}},
	}
	if !hasItemFor(items, once) {
		t.Error("a one-date event cited at another time was not found")
	}
	if hasItemFor(items, weekly) {
		t.Error("last week's item was taken for this week's date")
	}
	items = append(items, timelineItem{ID: "z", OccurredAt: 1000000 + 3600, Sources: []string{"discord_event_000002"}})
	if !hasItemFor(items, weekly) {
		t.Error("this week's item was not found")
	}
}

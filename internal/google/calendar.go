package google

import (
	"context"
	"net/url"
	"strings"
	"time"
)

// Event is a calendar event flattened for display in chat.
type Event struct {
	Summary   string
	Start     time.Time
	End       time.Time
	AllDay    bool
	Location  string
	Attendees []string
	HangoutsLink string
	HTMLLink  string
}

// rawEvents mirrors the bits of the Calendar API events:list response we use.
type rawEvents struct {
	Items []struct {
		Summary  string `json:"summary"`
		Location string `json:"location"`
		HTMLLink string `json:"htmlLink"`
		HangoutsLink string `json:"hangoutLink"`
		Start    struct {
			DateTime string `json:"dateTime"`
			Date     string `json:"date"`
		} `json:"start"`
		End struct {
			DateTime string `json:"dateTime"`
			Date     string `json:"date"`
		} `json:"end"`
		Attendees []struct {
			Email string `json:"email"`
		} `json:"attendees"`
	} `json:"items"`
}

// EventsBetween returns the primary calendar's events in [from, to),
// expanding recurring events and ordered by start time.
func (c *Client) EventsBetween(ctx context.Context, from, to time.Time) ([]Event, error) {
	v := url.Values{}
	v.Set("timeMin", from.Format(time.RFC3339))
	v.Set("timeMax", to.Format(time.RFC3339))
	v.Set("singleEvents", "true")
	v.Set("orderBy", "startTime")
	v.Set("maxResults", "50")
	u := "https://www.googleapis.com/calendar/v3/calendars/primary/events?" + v.Encode()
	var raw rawEvents
	if err := c.getJSON(ctx, u, &raw); err != nil {
		return nil, err
	}
	out := make([]Event, 0, len(raw.Items))
	for _, it := range raw.Items {
		e := Event{Summary: it.Summary, Location: it.Location, HTMLLink: it.HTMLLink, HangoutsLink: it.HangoutsLink}
		if it.Start.DateTime != "" {
			if t, err := time.Parse(time.RFC3339, it.Start.DateTime); err == nil {
				e.Start = t
			}
		} else if it.Start.Date != "" {
			e.AllDay = true
			if t, err := time.Parse("2006-01-02", it.Start.Date); err == nil {
				e.Start = t
			}
		}
		if it.End.DateTime != "" {
			if t, err := time.Parse(time.RFC3339, it.End.DateTime); err == nil {
				e.End = t
			}
		}
		for _, a := range it.Attendees {
			if a.Email != "" {
				e.Attendees = append(e.Attendees, a.Email)
			}
		}
		if strings.TrimSpace(e.Summary) == "" {
			e.Summary = "(no title)"
		}
		out = append(out, e)
	}
	return out, nil
}

// EventsToday returns today's events in the local timezone.
func (c *Client) EventsToday(ctx context.Context) ([]Event, error) {
	now := time.Now()
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	end := start.AddDate(0, 0, 1)
	return c.EventsBetween(ctx, start, end)
}

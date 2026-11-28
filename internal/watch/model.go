package watch

import "time"

// Seen is one variant's stock streak, in any watched DC. Last is NULL in the row while it is in stock at the last
// poll (meta.polled_at).
//
// Table: seen
type Seen struct {
	Since time.Time // the current or last streak began
	Last  time.Time // last poll that saw it in stock
	In    bool      // in stock at the last poll
}

// Span is a stretch of time: a pair in stock (table span), or polls running (table polled, From and To only). Slow:
// orderable with 72H delivery rather than 1H. Gone is the poll that found it sold out; zero while open or when
// polling stopped. Quiet: nobody saw it come in (first poll after a start, after a gap), so the log has no "in" for
// it. To is NULL in the row while open at the last poll (meta.polled_at).
//
// Table: span
type Span struct {
	From  time.Time
	To    time.Time
	Gone  time.Time
	Slow  bool
	Quiet bool
}

// Price is a variant's catalog price from At on.
//
// Table: price
type Price struct {
	At    time.Time
	Rent  float64
	Setup float64
}

// Event is a restock or a weekly digest, whether a channel took it or not. At and Kind name it: one of each per poll
// at most.
//
// Table: event
type Event struct {
	At    time.Time
	Kind  string
	Title string
	Body  string // the push text, full order links included: a mail or ntfy client needs them
	// Delivery is "sent", "no channel", "failed on …" or "partly failed on …", naming channels by scheme only: the
	// page has no login, a URL may hold a secret.
	Delivery string
	// Retry: no channel took it; sent again each poll while any of its pairs is still in stock.
	Retry bool
	// Items are a restock's servers as they were then, for the page, as JSON. None on a digest or an imported push.
	Items []Item
}

// Item is one server of a restock event, kept as JSON in event.items.
//
// Table: -
type Item struct {
	FQN      string   `json:"fqn"`
	Name     string   `json:"name,omitempty"`
	PlanCode string   `json:"plan"`
	CPU      string   `json:"cpu,omitempty"`
	RAM      int      `json:"ram"`
	Disks    []string `json:"disks,omitempty"`
	Rent     float64  `json:"rent,omitempty"`
	Priced   bool     `json:"priced,omitempty"`
	OrderURL string   `json:"order,omitempty"`
	DCs      []ItemDC `json:"dcs"`
}

// ItemDC is a DC a restock item came in at; Slow: 72H delivery.
//
// Table: -
type ItemDC struct {
	DC   string `json:"dc"`
	Slow bool   `json:"slow,omitempty"`
}

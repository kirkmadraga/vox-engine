package commands

import (
	"context"
	"slices"
	"testing"

	"github.com/disgoorg/snowflake/v2"
)

type fakeReplier struct{ got []Reply }

func (f *fakeReplier) Reply(_ context.Context, r Reply) error {
	f.got = append(f.got, r)
	return nil
}

func TestPing(t *testing.T) {
	rep := &fakeReplier{}
	err := Ping{}.Run(context.Background(), Request{AuthorID: 42, Reply: rep})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rep.got) != 1 {
		t.Fatalf("got %d replies, want 1", len(rep.got))
	}
	r := rep.got[0]
	if r.Content != "<@42> pong" {
		t.Errorf("Content = %q", r.Content)
	}
	if !slices.Equal(r.Mentions, []snowflake.ID{42}) {
		t.Errorf("Mentions = %v, want only the author", r.Mentions)
	}
}

type named string

func (n named) Name() string                     { return string(n) }
func (named) Run(context.Context, Request) error { return nil }

func TestRegistry(t *testing.T) {
	r, err := NewRegistry(Ping{})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ping", "PING", "Ping"} {
		if _, ok := r.Lookup(name); !ok {
			t.Errorf("Lookup(%q) not found", name)
		}
	}
	if _, ok := r.Lookup("pong"); ok {
		t.Error("Lookup(pong) should not be found")
	}

	if _, err := NewRegistry(named("a"), named("A")); err == nil {
		t.Error("duplicate names (case-insensitive) should fail")
	}
	if _, err := NewRegistry(named("")); err == nil {
		t.Error("empty name should fail")
	}
}

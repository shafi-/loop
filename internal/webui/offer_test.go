package webui

import (
	"testing"

	"github.com/shafi-/loop/internal/daemon"
)

// The room engine's deep-dive offer marker renders as an offer card
// carrying the agent and the command its button posts.
func TestChatViewRendersDiveOffer(t *testing.T) {
	lines := []daemon.RoomLine{
		{Seq: 1, From: "architect", Text: "My read: sessions leak."},
		{Seq: 2, From: "system", Text: "◈ deep-dive offer @architect — topic: session invalidation in auth/ — run: /deepdive @architect"},
		{Seq: 3, From: "system", Text: "plain system line"},
	}
	chat := ChatView(lines, []string{"architect"}, nil)
	if len(chat) != 3 {
		t.Fatalf("messages = %d", len(chat))
	}
	offer := chat[1]
	if offer.Kind != "offer" || offer.From != "architect" {
		t.Fatalf("offer = %+v", offer)
	}
	if offer.Text != "session invalidation in auth/" {
		t.Errorf("topic = %q", offer.Text)
	}
	if offer.Detail != "/deepdive @architect" {
		t.Errorf("cta command = %q", offer.Detail)
	}
	if chat[2].Kind != "system" {
		t.Errorf("plain system line misclassified: %+v", chat[2])
	}
}

package lab

import (
	"slices"
	"testing"

	"github.com/giantswarm/agentlab/internal/config"
	"github.com/giantswarm/agentlab/internal/forms"
)

func TestDecideTrust(t *testing.T) {
	for _, tc := range []struct {
		trusted, terminal bool
		want              trustStep
	}{
		{true, true, trustNothing},
		{true, false, trustNothing},
		{false, true, trustAsk},
		{false, false, trustWarn},
	} {
		if got := decideTrust(tc.trusted, tc.terminal); got != tc.want {
			t.Errorf("decideTrust(%v, %v) = %v, want %v", tc.trusted, tc.terminal, got, tc.want)
		}
	}
}

// ptr is the shape of a pre-answered --trust/--open flag.
func ptr(b bool) *bool { return &b }

// The two questions a boot ends with: asked on a terminal, silent off one,
// and skipped entirely where a flag already answered.
func TestOfferTrustAndOpen(t *testing.T) {
	for _, tc := range []struct {
		name             string
		trusted          bool
		terminal         bool
		answer           bool
		backstage        bool
		portalUp         bool
		offers           Offers
		wantAsked        int
		wantTrusted      int
		wantOpened       bool
		wantAnswerAborts bool
	}{
		{name: "untrusted on a terminal: both questions, yes to both",
			terminal: true, answer: true, backstage: true, portalUp: true,
			wantAsked: 2, wantTrusted: 1, wantOpened: true},
		{name: "already trusted: only the open question",
			trusted: true, terminal: true, answer: true, backstage: true, portalUp: true,
			wantAsked: 1, wantOpened: true},
		{name: "no to both: nothing happens",
			terminal: true, backstage: true, portalUp: true,
			wantAsked: 2},
		{name: "off a terminal: nothing is asked",
			backstage: true, portalUp: true},
		{name: "an unreachable portal is not offered",
			trusted: true, terminal: true, answer: true, backstage: true,
			wantAsked: 0},
		{name: "no Backstage, no open question",
			terminal: true, answer: true, portalUp: true,
			wantAsked: 1, wantTrusted: 1},
		{name: "--trust --open answer for a scripted run off a terminal",
			backstage: true, portalUp: true, offers: Offers{Trust: ptr(true), Open: ptr(true)},
			wantTrusted: 1, wantOpened: true},
		{name: "--trust=false --open=false ask nothing",
			terminal: true, backstage: true, portalUp: true,
			offers: Offers{Trust: ptr(false), Open: ptr(false)}},
		{name: "Ctrl-C on the trust question stops the offers",
			terminal: true, backstage: true, portalUp: true, wantAnswerAborts: true,
			wantAsked: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var answerErr error
			if tc.wantAnswerAborts {
				answerErr = forms.ErrAborted
			}
			s := stubOpen(t, tc.trusted, tc.terminal, tc.answer, answerErr, "running", nil, true, true)
			cfg := config.Default()
			cfg.Backstage.Enabled = tc.backstage

			if err := offerTrustAndOpen(cfg, tc.offers, tc.portalUp); err != nil {
				t.Fatalf("offerTrustAndOpen: %v", err)
			}
			if s.asked != tc.wantAsked {
				t.Errorf("asked %d questions, want %d", s.asked, tc.wantAsked)
			}
			if s.trusted != tc.wantTrusted {
				t.Errorf("ran trust %d times, want %d", s.trusted, tc.wantTrusted)
			}
			want := []string(nil)
			if tc.wantOpened {
				want = []string{cfg.BackstageBaseURL()}
			}
			if !slices.Equal(s.opened, want) {
				t.Errorf("opened %v, want %v", s.opened, want)
			}
		})
	}
}

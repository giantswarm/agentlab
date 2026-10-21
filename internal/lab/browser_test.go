package lab

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const signInFailedHeading = "<h1>Sign-in failed</h1>"

func TestBrowserCallbackPages(t *testing.T) {
	const state = "lab-state"
	tests := []struct {
		name       string
		query      string
		wantStatus int
		wantCode   string
		want       []string
		notWant    []string
	}{
		{
			name:       "signed in",
			query:      "code=c&state=" + state,
			wantStatus: http.StatusOK,
			wantCode:   "c",
			want:       []string{"<h1>Signed in</h1>", "agentlab is now signed in", "return to the terminal"},
		},
		{
			name:       "Dex refuses",
			query:      "error=access_denied&error_description=%3Cb%3Eno%3C%2Fb%3E&state=" + state,
			wantStatus: http.StatusBadRequest,
			want:       []string{signInFailedHeading, "The sign-in was denied or failed", "<code>access_denied</code>"},
			notWant:    []string{"<b>no</b>"},
		},
		{
			name:       "state mismatch",
			query:      "code=c&state=forged",
			wantStatus: http.StatusBadRequest,
			want:       []string{signInFailedHeading, "The sign-in response could not be verified"},
		},
		{
			name:       "no code",
			query:      "state=" + state,
			wantStatus: http.StatusBadRequest,
			want:       []string{signInFailedHeading, "The sign-in response is incomplete"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			codeCh := make(chan string, 1)
			rr := httptest.NewRecorder()

			browserCallback(state, codeCh)(rr, httptest.NewRequest(http.MethodGet, "/callback?"+tc.query, nil))

			if rr.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", rr.Code, tc.wantStatus)
			}
			body := rr.Body.String()
			for _, want := range tc.want {
				if !strings.Contains(body, want) {
					t.Errorf("body does not contain %q: %s", want, body)
				}
			}
			for _, notWant := range tc.notWant {
				if strings.Contains(body, notWant) {
					t.Errorf("body contains %q", notWant)
				}
			}
			select {
			case code := <-codeCh:
				if code != tc.wantCode {
					t.Errorf("code = %q, want %q", code, tc.wantCode)
				}
			default:
				if tc.wantCode != "" {
					t.Errorf("no code handed over, want %q", tc.wantCode)
				}
			}
		})
	}
}

// TestBrowserCallbackRepeated covers a second load of the callback: it gets a
// page and does not block on the channel the first code already filled.
func TestBrowserCallbackRepeated(t *testing.T) {
	codeCh := make(chan string, 1)
	handler := browserCallback("s", codeCh)

	handler(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/callback?code=first&state=s", nil))
	rr := httptest.NewRecorder()
	handler(rr, httptest.NewRequest(http.MethodGet, "/callback?code=second&state=s", nil))

	if rr.Code != http.StatusOK {
		t.Errorf("second load: status %d", rr.Code)
	}
	if code := <-codeCh; code != "first" {
		t.Errorf("code = %q, want the first one", code)
	}
}

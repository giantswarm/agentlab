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
		// wantCode and wantErr describe what BrowserLogin is handed; with
		// neither, it is handed nothing and keeps waiting.
		wantCode string
		wantErr  string
		want     []string
		notWant  []string
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
			wantErr:    "sign-in refused by Dex: access_denied (<b>no</b>)",
			want:       []string{signInFailedHeading, "The sign-in was denied or failed", "<code>access_denied</code>", "run the login again"},
			notWant:    []string{"<b>no</b>"},
		},
		{
			name:       "Dex error that is not a registered code",
			query:      "error=your_account_is_locked_call_555&state=" + state,
			wantStatus: http.StatusBadRequest,
			wantErr:    "your_account_is_locked_call_555",
			want:       []string{"The sign-in was denied or failed"},
			notWant:    []string{"your_account_is_locked"},
		},
		{
			name:       "error without this sign-in's state",
			query:      "error=access_denied",
			wantStatus: http.StatusBadRequest,
			want:       []string{signInFailedHeading, "The sign-in response could not be verified"},
			notWant:    []string{"access_denied"},
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
			wantErr:    "no code",
			want:       []string{signInFailedHeading, "The sign-in response is incomplete"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			results := make(chan callbackResult, 1)
			rr := httptest.NewRecorder()

			browserCallback(state, results)(rr, httptest.NewRequest(http.MethodGet, "/callback?"+tc.query, nil))

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
			case res := <-results:
				if res.code != tc.wantCode {
					t.Errorf("code = %q, want %q", res.code, tc.wantCode)
				}
				switch {
				case tc.wantErr == "" && res.err != nil:
					t.Errorf("unexpected error: %v", res.err)
				case tc.wantErr != "" && (res.err == nil || !strings.Contains(res.err.Error(), tc.wantErr)):
					t.Errorf("error = %v, want one containing %q", res.err, tc.wantErr)
				}
			default:
				if tc.wantCode != "" || tc.wantErr != "" {
					t.Error("nothing handed to BrowserLogin")
				}
			}
		})
	}
}

// TestBrowserCallbackErrorForTheTerminal covers the description a refused
// sign-in reports in the terminal: control characters, which could move the
// cursor or rewrite the terminal, are dropped.
func TestBrowserCallbackErrorForTheTerminal(t *testing.T) {
	results := make(chan callbackResult, 1)

	browserCallback("s", results)(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet,
		"/callback?error=access_denied&error_description=%1B%5B2Jno%07&state=s", nil))

	res := <-results
	if res.err == nil || strings.ContainsAny(res.err.Error(), "\x1b\x07") {
		t.Errorf("error = %q, want one without control characters", res.err)
	}
}

// TestBrowserCallbackRepeated covers a second load of the callback: it gets a
// page and does not block on the channel the first code already filled.
func TestBrowserCallbackRepeated(t *testing.T) {
	results := make(chan callbackResult, 1)
	handler := browserCallback("s", results)

	handler(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/callback?code=first&state=s", nil))
	rr := httptest.NewRecorder()
	handler(rr, httptest.NewRequest(http.MethodGet, "/callback?code=second&state=s", nil))

	if rr.Code != http.StatusOK {
		t.Errorf("second load: status %d", rr.Code)
	}
	select {
	case res := <-results:
		if res.code != "first" {
			t.Errorf("code = %q, want the first one", res.code)
		}
	default:
		t.Error("nothing handed to BrowserLogin")
	}
}

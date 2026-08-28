// Package tui is the interactive dashboard for agentlab: a live status view
// of every component plus the lifecycle actions. Actions run this same binary
// as a subprocess (`agentlab up`, `agentlab down`, ...) with their output
// streamed into a scrollback pane — the lab functions print plain text to
// stdout, and a subprocess keeps that intact without fighting bubbletea over
// the terminal.
package tui

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"agentlab/internal/config"
	"agentlab/internal/lab"
)

const (
	probeInterval  = 3 * time.Second
	maxOutputLines = 5000
)

// Run starts the dashboard and blocks until the user quits.
func Run(cfg *config.Config) error {
	_, err := tea.NewProgram(newModel(cfg), tea.WithAltScreen()).Run()
	return err
}

// mode is which key handler owns the next keypress; every non-main mode is a
// one-shot submenu rendered in the footer.
type mode int

const (
	modeMain mode = iota
	modeConfirmDown
	modeOpen
	modeLogs
)

type (
	// tickMsg fires the periodic status probe.
	tickMsg struct{}
	// statusMsg carries a finished probe, plus the (possibly reloaded)
	// config it was probed against.
	statusMsg struct {
		status lab.Status
		cfg    *config.Config
		mtime  time.Time
	}
	// actionStartedMsg reports a successfully spawned subprocess.
	actionStartedMsg struct {
		name   string
		ch     chan actionEvent
		cancel context.CancelFunc
	}
	// actionFailedMsg reports a subprocess that never started.
	actionFailedMsg struct{ err error }
	// actionEvent is one output line, or (done) the exit of the subprocess.
	actionEvent struct {
		line     string
		done     bool
		canceled bool // the user canceled; err is just the kill signal then
		err      error
	}
)

type model struct {
	cfg      *config.Config
	cfgMTime time.Time

	status  lab.Status
	probed  bool // first probe answered
	probing bool

	mode      mode
	showUsers bool
	flash     string // one-line transient notice in the footer

	action string // running action's label; "" when idle
	cancel context.CancelFunc
	events chan actionEvent
	output []string

	spin   spinner.Model
	vp     viewport.Model
	ready  bool // first WindowSizeMsg arrived
	width  int
	height int

	// runner spawns an action; swapped out in tests.
	runner func(label string, args ...string) tea.Cmd
}

func newModel(cfg *config.Config) model {
	sp := spinner.New()
	sp.Spinner = spinner.MiniDot
	sp.Style = styleKey
	m := model{cfg: cfg, spin: sp, runner: startAction}
	if fi, err := os.Stat(config.File); err == nil {
		m.cfgMTime = fi.ModTime()
	}
	m.output = []string{styleSubtle.Render(
		"Actions run `agentlab <command>` as a subprocess; output lands here. Scroll with ↑/↓, pgup/pgdn.")}
	return m
}

func (m model) Init() tea.Cmd {
	return tea.Batch(probe(m.cfg, m.cfgMTime), tick())
}

func tick() tea.Cmd {
	return tea.Tick(probeInterval, func(time.Time) tea.Msg { return tickMsg{} })
}

// probe re-reads the config when agentlab.yaml changed on disk (so external
// edits show up live) and then checks every component.
func probe(cfg *config.Config, mtime time.Time) tea.Cmd {
	return func() tea.Msg {
		msg := statusMsg{cfg: cfg, mtime: mtime}
		if fi, err := os.Stat(config.File); err == nil && !fi.ModTime().Equal(mtime) {
			if fresh, err := config.Load(); err == nil {
				msg.cfg = fresh
			}
			msg.mtime = fi.ModTime()
		}
		msg.status = lab.Probe(msg.cfg)
		return msg
	}
}

// selfExe resolves the binary that actions re-invoke; a variable so tests can
// point it at a shell instead of the test binary.
var selfExe = os.Executable

// startAction spawns this same binary with the given args and streams its
// combined output, line by line, into the returned channel; the done event is
// always last.
func startAction(label string, args ...string) tea.Cmd {
	return func() tea.Msg {
		self, err := selfExe()
		if err != nil {
			return actionFailedMsg{err}
		}
		ctx, cancel := context.WithCancel(context.Background())
		cmd := exec.CommandContext(ctx, self, args...)
		// The subprocess spawns children of its own (kubectl logs -f, helm,
		// kind). Killing just the direct child would leave those holding the
		// output pipe — and cmd.Wait blocked on it forever — so each action
		// gets its own process group and cancel kills the whole group.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error {
			// Group gone or signal denied: fall back to the direct child.
			// Process.Kill reports os.ErrProcessDone if it already exited.
			if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); err != nil {
				return cmd.Process.Kill()
			}
			return nil
		}
		// Backstop for a child that ignores SIGTERM or escapes the group:
		// stop waiting on the pipes shortly after the process is dead.
		cmd.WaitDelay = 2 * time.Second
		pr, pw := io.Pipe()
		cmd.Stdout = pw
		cmd.Stderr = pw
		if err := cmd.Start(); err != nil {
			cancel()
			return actionFailedMsg{err}
		}
		waitErr := make(chan error, 1)
		go func() {
			// Wait returns once the exec copiers have written everything into
			// pw; closing it then EOFs the scanner, so the done event follows
			// the last line.
			waitErr <- cmd.Wait()
			pw.Close()
		}()
		ch := make(chan actionEvent, 64)
		go func() {
			sc := bufio.NewScanner(pr)
			sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
			for sc.Scan() {
				ch <- actionEvent{line: sc.Text()}
			}
			ch <- actionEvent{done: true, err: <-waitErr, canceled: ctx.Err() != nil}
		}()
		return actionStartedMsg{name: label, ch: ch, cancel: cancel}
	}
}

func listen(ch chan actionEvent) tea.Cmd {
	return func() tea.Msg { return <-ch }
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.resize()
		return m, nil

	case tickMsg:
		cmds := []tea.Cmd{tick()}
		if !m.probing {
			m.probing = true
			cmds = append(cmds, probe(m.cfg, m.cfgMTime))
		}
		return m, tea.Batch(cmds...)

	case statusMsg:
		m.probing = false
		m.probed = true
		m.status = msg.status
		m.cfg = msg.cfg
		m.cfgMTime = msg.mtime
		m.resize() // the top pane's height depends on config (user count)
		return m, nil

	case actionStartedMsg:
		m.action = msg.name
		m.cancel = msg.cancel
		m.events = msg.ch
		return m, tea.Batch(listen(msg.ch), m.spin.Tick)

	case actionFailedMsg:
		m.appendLine(styleBad.Render("could not start: " + msg.err.Error()))
		return m, nil

	case actionEvent:
		if msg.done {
			switch {
			case msg.canceled:
				m.appendLine(styleWarn.Render("✕ " + m.action + " canceled"))
			case msg.err != nil:
				m.appendLine(styleBad.Render("✗ " + m.action + ": " + msg.err.Error()))
			default:
				m.appendLine(styleGood.Render("✓ " + m.action + " finished"))
			}
			m.action = ""
			m.cancel = nil
			m.events = nil
			m.probing = true
			return m, probe(m.cfg, m.cfgMTime)
		}
		m.appendLine(msg.line)
		return m, listen(m.events)

	case spinner.TickMsg:
		if m.action == "" {
			return m, nil
		}
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd

	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m model) handleKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := k.String()
	m.flash = ""

	switch m.mode {
	case modeConfirmDown:
		switch key {
		case "y", "enter":
			m.mode = modeMain
			return m.launch("down", "down")
		case "n", "esc", "q", "ctrl+c":
			m.mode = modeMain
		}
		return m, nil

	case modeOpen:
		m.mode = modeMain
		switch key {
		case "d":
			m.openURL(m.cfg.Issuer() + "/.well-known/openid-configuration")
		case "m":
			if m.cfg.Platform.Enabled {
				m.openURL(m.cfg.MusterBaseURL())
			} else {
				m.flash = "the platform is disabled in " + config.File
			}
		case "a":
			if m.cfg.Platform.Enabled && m.cfg.Platform.Agents {
				m.openURL(m.cfg.KagentUIBaseURL())
			} else {
				m.flash = "agents are disabled in " + config.File
			}
		case "b":
			if m.cfg.Backstage.Enabled {
				m.openURL(m.cfg.BackstageBaseURL())
			} else {
				m.flash = "backstage is disabled in " + config.File
			}
		case "esc", "q", "ctrl+c":
		default:
			m.mode = modeOpen // unknown key: stay in the submenu
		}
		return m, nil

	case modeLogs:
		m.mode = modeMain
		switch key {
		case "d":
			return m.launch("logs dex", "logs", "dex")
		case "m":
			return m.launch("logs muster", "logs", "muster")
		case "b":
			return m.launch("logs backstage", "logs", "backstage")
		case "esc", "q", "ctrl+c":
		default:
			m.mode = modeLogs
		}
		return m, nil
	}

	switch key {
	case "ctrl+c":
		if m.cancel != nil {
			m.cancel()
		}
		return m, tea.Quit
	case "q":
		if m.action != "" {
			m.flash = m.action + " is still running — [x] cancels it, ctrl+c force-quits"
			return m, nil
		}
		return m, tea.Quit
	case "x":
		if m.cancel != nil {
			m.cancel()
			m.flash = "canceling " + m.action + "…"
		}
		return m, nil
	case "s":
		return m.launch("up", "up")
	case "d":
		if m.action != "" {
			m.flash = m.action + " is still running — [x] cancels it"
			return m, nil
		}
		m.mode = modeConfirmDown
		return m, nil
	case "t":
		return m.launch("test", "test")
	case "p":
		if !m.cfg.Platform.Enabled {
			m.flash = "the platform is disabled in " + config.File
			return m, nil
		}
		return m.launch("platform-test", "platform-test")
	case "b":
		return m.launch("browser login", "browser")
	case "l":
		m.mode = modeLogs
		return m, nil
	case "o":
		m.mode = modeOpen
		return m, nil
	case "u":
		m.showUsers = !m.showUsers
		m.resize()
		return m, nil
	case "c":
		if wd, err := os.Getwd(); err == nil {
			lab.OpenBrowser(wd)
			m.flash = "opened " + wd
		}
		return m, nil
	case "r":
		if !m.probing {
			m.probing = true
			return m, probe(m.cfg, m.cfgMTime)
		}
		return m, nil
	case "up", "down", "pgup", "pgdown":
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(k)
		return m, cmd
	case "home":
		m.vp.GotoTop()
		return m, nil
	case "end":
		m.vp.GotoBottom()
		return m, nil
	}
	return m, nil
}

// launch starts an action subprocess unless one is already running (the lab
// steps share cwd state; two at once would interleave kubectl contexts).
func (m model) launch(label string, args ...string) (tea.Model, tea.Cmd) {
	if m.action != "" {
		m.flash = m.action + " is still running — [x] cancels it"
		return m, nil
	}
	m.appendLine("", stylePrompt.Render("$ agentlab "+strings.Join(args, " ")))
	return m, m.runner(label, args...)
}

func (m *model) openURL(url string) {
	lab.OpenBrowser(url)
	m.flash = "opened " + url
}

func (m *model) appendLine(lines ...string) {
	m.output = append(m.output, lines...)
	if len(m.output) > maxOutputLines {
		m.output = append([]string(nil), m.output[len(m.output)-maxOutputLines:]...)
	}
	m.syncViewport()
}

// syncViewport re-renders the scrollback into the viewport, hard-wrapping to
// the current width, and keeps following the tail unless the user scrolled up.
func (m *model) syncViewport() {
	if !m.ready {
		return
	}
	atBottom := m.vp.AtBottom()
	var b strings.Builder
	for i, l := range m.output {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(ansi.Hardwrap(l, m.vp.Width, true))
	}
	m.vp.SetContent(b.String())
	if atBottom {
		m.vp.GotoBottom()
	}
}

// resize fits the viewport into whatever the top and footer panes leave over.
func (m *model) resize() {
	if m.width == 0 {
		return
	}
	h := max(m.height-lipgloss.Height(m.topView())-lipgloss.Height(m.footView())-1, 3) // -1: output rule
	if !m.ready {
		m.vp = viewport.New(m.width, h)
		m.ready = true
	} else {
		m.vp.Width = m.width
		m.vp.Height = h
	}
	m.syncViewport()
}

var (
	styleTitle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("231")).Background(lipgloss.Color("62")).Padding(0, 1)
	styleSubtle = lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
	styleGood   = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	styleBad    = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	styleOff    = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	styleKey    = lipgloss.NewStyle().Foreground(lipgloss.Color("205"))
	stylePrompt = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("81"))
	styleWarn   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("214"))
	styleRule   = lipgloss.NewStyle().Foreground(lipgloss.Color("238"))
)

func (m model) View() string {
	if !m.ready {
		return "starting…"
	}
	rule := styleRule.Render(strings.Repeat("─", max(m.width, 1)))
	return strings.Join([]string{m.topView(), rule, m.vp.View(), m.footView()}, "\n")
}

// topView renders the header, the component status table and (optionally) the
// user list. Its height feeds resize, so View and resize must agree.
func (m model) topView() string {
	var b strings.Builder
	b.WriteString(styleTitle.Render("agentlab") + "  " +
		styleSubtle.Render(config.File+" · cluster "+m.cfg.ClusterName) + "\n\n")

	b.WriteString(m.statusLine("kind cluster", m.cfg.KubeContext(), true, m.status.ClusterUp))
	b.WriteString(m.statusLine("dex", m.cfg.Issuer(), true, m.status.DexUp))
	b.WriteString(m.statusLine("agent platform", m.cfg.MusterBaseURL(), m.cfg.Platform.Enabled, m.status.MusterUp))
	b.WriteString(m.statusLine("agents (kagent)", m.cfg.KagentUIBaseURL(),
		m.cfg.Platform.Enabled && m.cfg.Platform.Agents, m.status.AgentsUp))
	b.WriteString(m.statusLine("backstage", m.cfg.BackstageBaseURL(), m.cfg.Backstage.Enabled, m.status.BackstageUp))

	files := "  " + fileMark("certs/", m.status.CertsPresent) + "   " +
		fileMark("kubeconfig.oidc", m.status.KubeconfigPresent)
	if m.status.KubeconfigPresent {
		files += styleSubtle.Render("   (KUBECONFIG=$PWD/kubeconfig.oidc)")
	}
	b.WriteString(files + "\n")
	if m.status.MusterUp {
		b.WriteString(styleSubtle.Render("  Claude Code: claude mcp add --transport http muster "+
			m.cfg.MusterBaseURL()+"/mcp") + "\n")
	}

	if m.showUsers {
		b.WriteString("\n" + m.usersView())
	}
	return strings.TrimRight(b.String(), "\n")
}

func (m model) statusLine(name, detail string, enabled, up bool) string {
	dot, state := styleOff.Render("◌"), styleOff.Render("disabled")
	switch {
	case !enabled:
	case !m.probed:
		dot, state = styleSubtle.Render("●"), styleSubtle.Render("probing…")
	case up:
		dot, state = styleGood.Render("●"), styleGood.Render("up")
	default:
		dot, state = styleBad.Render("●"), styleBad.Render("down")
	}
	return fmt.Sprintf("  %s %-15s %s %s\n", dot, name,
		styleSubtle.Render(fmt.Sprintf("%-42s", detail)), state)
}

func fileMark(name string, present bool) string {
	if present {
		return styleGood.Render("✓ ") + styleSubtle.Render(name)
	}
	return styleOff.Render("✗ " + name)
}

func (m model) usersView() string {
	emailW, userW, passW := 0, 0, 0
	for _, u := range m.cfg.Users {
		emailW = max(emailW, len(u.Email))
		userW = max(userW, len(u.Username))
		passW = max(passW, len(u.Password))
	}
	var b strings.Builder
	b.WriteString("  " + styleSubtle.Render(fmt.Sprintf("%-*s  %-*s  %-*s  %s",
		emailW, "EMAIL", userW, "USER", passW, "PASSWORD", "GROUPS")) + "\n")
	for _, u := range m.cfg.Users {
		fmt.Fprintf(&b, "  %-*s  %-*s  %-*s  %s\n",
			emailW, u.Email, userW, u.Username, passW, u.Password,
			styleSubtle.Render(strings.Join(u.Groups, ", ")))
	}
	return b.String()
}

func (m model) footView() string {
	var line string
	switch m.mode {
	case modeConfirmDown:
		line = styleWarn.Render("destroy the kind cluster \""+m.cfg.ClusterName+"\"?") + "  " +
			keyHelp("y", "yes") + "  " + keyHelp("n", "no")
	case modeOpen:
		line = "open in browser: " + keyHelp("d", "dex discovery") + "  " +
			keyHelp("m", "muster") + "  " + keyHelp("a", "agents ui") + "  " +
			keyHelp("b", "backstage") + "  " + keyHelp("esc", "back")
	case modeLogs:
		line = "tail logs (follows until [x]): " + keyHelp("d", "dex") + "  " +
			keyHelp("m", "muster") + "  " + keyHelp("b", "backstage") + "  " + keyHelp("esc", "back")
	default:
		if m.action != "" {
			line = m.spin.View() + " running " + stylePrompt.Render(m.action) + "…  " +
				keyHelp("x", "cancel") + "  " + keyHelp("u", "users") + "  " +
				keyHelp("o", "open url") + "  " + keyHelp("c", "config dir")
		} else {
			line = keyHelp("s", "up") + "  " + keyHelp("d", "down") + "  " +
				keyHelp("t", "rbac test") + "  " + keyHelp("p", "platform test") + "  " +
				keyHelp("b", "browser login") + "  " + keyHelp("l", "logs") + "  " +
				keyHelp("o", "open url") + "  " + keyHelp("u", "users") + "  " +
				keyHelp("c", "config dir") + "  " + keyHelp("r", "refresh") + "  " +
				keyHelp("q", "quit")
		}
	}
	flash := ""
	if m.flash != "" {
		flash = styleWarn.Render(m.flash)
	}
	return flash + "\n " + line
}

func keyHelp(key, label string) string {
	return styleKey.Render("["+key+"]") + " " + styleSubtle.Render(label)
}

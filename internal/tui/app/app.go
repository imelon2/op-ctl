// Package app is the unified bubbletea program that drives the
// interactive `./op-ctl` flow. Every transition (menu → submenu →
// backend picker → peers, or list / namespace output) happens inside a
// single tea.Program with a single alt-screen, so the operator never
// sees a flicker of the underlying terminal between screens.
//
// Architecture: App holds a stack of tea.Model "screens". Update()
// forwards keystrokes to the top screen and intercepts app-level
// navigation messages (popMsg, cmdSelectedMsg, backendSelectedMsg,
// async-result messages) to mutate the stack or kick off async work.
// Screens themselves are simple wrappers around the existing
// menu / peers / errscreen models — those models were refactored to
// emit typed messages instead of calling tea.Quit so they could be
// embedded here.
package app

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"

	"op-ctl/internal/config"
	"op-ctl/internal/elnode"
	"op-ctl/internal/namespace"
	"op-ctl/internal/opnode"
	"op-ctl/internal/sshtunnel"
	"op-ctl/internal/tui/errscreen"
	"op-ctl/internal/tui/menu"
	peerstui "op-ctl/internal/tui/peers"
)

// backendKind tags a backend menu so the same selector widget can be
// reused for every per-backend flow. The selection handler dispatches
// on kind to decide which RPC fan-out to launch.
type backendKind int

const (
	backendForConsensus          backendKind = iota // p2p > peer > consensus (opp2p_peers)
	backendForExecution                             // p2p > peer > execution (admin_peers)
	backendForDiscoveryConsensus                    // p2p > discovery > consensus (opp2p_discoveryTable)
)

// ---------- app-level messages ----------

type popMsg struct{}
type cmdSelectedMsg struct{ name string }
type backendSelectedMsg struct {
	backend config.Backend
	kind    backendKind
}

type peersFetchedMsg struct {
	backend config.Backend
	dump    *opnode.PeerDump
	err     error
}

type peerDetailMsg struct {
	backend config.Backend
	peerID  string
	name    string
	entry   *opnode.PeerEntry
	banned  bool
}

type executionPeersFetchedMsg struct {
	backend  config.Backend
	count    uint64
	countErr error
	peers    []elnode.AdminPeer
	peersErr error
}

type executionPeerDetailMsg struct {
	backend config.Backend
	name    string
	peer    elnode.AdminPeer
}

type discoveryFetchedMsg struct {
	backend config.Backend
	enrs    []string
	err     error
}

type discoveryDetailMsg struct {
	backend config.Backend
	name    string
	enr     string
}

type listLoadedMsg struct {
	text string
	err  error
}

// txpool drill-down messages — emitted by the summary screen when the
// operator picks a backend and by the detail screen when they pick a
// specific tx. See status_txpool_screen.go and the two new detail
// screens for the receivers.
type txpoolDetailMsg struct {
	backend config.Backend
}

type txpoolListTickMsg struct{ gen uint64 }

type txpoolListFetchedMsg struct {
	gen        uint64
	txs        []elnode.TxPoolTx
	err        error
	observedAt time.Time
}

type txDetailMsg struct {
	backend config.Backend
	tx      *elnode.TxPoolTx
}

// configSelectedMsg is emitted by configPickerScreen when the operator
// picks one of the discovered *.toml files. App's handler runs the
// loader to materialize cfg + resolver + timeout, then replaces the
// picker on the stack with the root cobra menu.
type configSelectedMsg struct{ path string }

// ---------- App ----------

// App is the root tea.Model. It owns the screen stack and the shared
// configuration (config + namespace dir + timeout) that flow handlers
// need.
//
// When constructed via NewWithPicker, cfg/resolver/timeout start zero
// and the initial stack contains a configPickerScreen; handlers that
// depend on cfg/resolver only fire after the picker resolves and
// handleConfigSelected fills those fields in.
type App struct {
	cobra        *cobra.Command
	cfg          *config.Config
	resolver     *sshtunnel.Resolver
	namespaceDir string
	timeout      time.Duration

	loader configLoader // non-nil only on the picker path; see NewWithPicker.

	stack []tea.Model

	width  int
	height int
}

// configLoader materializes the App's runtime state from a chosen
// config path. Returning the resolved namespace directory + timeout
// alongside cfg/resolver lets the caller hold the per-chain dir
// derivation and the [global].namespace_timeout resolution rule
// (CLI > config > 5s default) outside the app package.
type configLoader func(path string) (cfg *config.Config, resolver *sshtunnel.Resolver, namespaceDir string, timeout time.Duration, err error)

// New constructs an App seeded with the root cmdMenu. cobra is the
// op-ctl root command — App walks Commands() to build menu items at
// each level. namespaceDir / timeout come from the flags on rootCmd
// (or their defaults). resolver routes per-backend RPC traffic through
// SSH bastions when a backend declares ssh_jump; nil disables routing.
func New(cobraRoot *cobra.Command, cfg *config.Config, resolver *sshtunnel.Resolver, namespaceDir string, timeout time.Duration) App {
	a := App{
		cobra:        cobraRoot,
		cfg:          cfg,
		resolver:     resolver,
		namespaceDir: namespaceDir,
		timeout:      timeout,
	}
	a.stack = []tea.Model{newCmdMenu(cobraRoot, "op-ctl")}
	return a
}

// Run launches the App in alt-screen mode and blocks until the user
// quits at the root level.
func Run(cobraRoot *cobra.Command, cfg *config.Config, resolver *sshtunnel.Resolver, namespaceDir string, timeout time.Duration) error {
	_, err := tea.NewProgram(New(cobraRoot, cfg, resolver, namespaceDir, timeout), tea.WithAltScreen()).Run()
	return err
}

// NewWithPicker constructs an App whose initial screen is a picker
// over `candidates`. After the operator selects one, the loader
// callback runs to produce cfg/resolver/namespaceDir/timeout and the
// picker is replaced with the root cobra menu — all inside one
// tea.Program so there is no alt-screen flicker between the two
// phases.
func NewWithPicker(cobraRoot *cobra.Command, candidates []string, loader configLoader) App {
	a := App{
		cobra:  cobraRoot,
		loader: loader,
	}
	a.stack = []tea.Model{newConfigPickerScreen(candidates)}
	return a
}

// RunWithPicker is the picker counterpart of Run. On success the
// resolver built inside the loader is closed before returning so
// callers don't need a separate cleanup defer for the picker path.
func RunWithPicker(ctx context.Context, cobraRoot *cobra.Command, candidates []string, loader configLoader) error {
	final, err := tea.NewProgram(
		NewWithPicker(cobraRoot, candidates, loader),
		tea.WithAltScreen(),
		tea.WithContext(ctx),
	).Run()
	if fa, ok := final.(App); ok && fa.resolver != nil {
		if cerr := fa.resolver.Close(); cerr != nil {
			// Mirror cmd/op-ctl/main.go's defer: surface but don't mask err.
			fmt.Fprintln(os.Stderr, "ssh resolver close:", cerr)
		}
	}
	return err
}

// RunStateBlock launches the state-block alt-screen as a standalone
// bubbletea program. This is the CLI-only entry point: invoked
// exclusively by stateBlockCmd.RunE when --plain is false. The unified
// App's menu path reuses newStateBlockScreen directly via a.push(...)
// and does NOT spawn a second tea.NewProgram (the App already owns the
// bubbletea loop), so two programs never contend for stdin/stdout.
func RunStateBlock(ctx context.Context, cfg *config.Config, resolver *sshtunnel.Resolver, interval, timeout time.Duration) error {
	screen := newStateBlockScreen(cfg.BackendList(), resolver, interval, timeout, cfg.Global.L2BlockTime)
	_, err := tea.NewProgram(screen, tea.WithAltScreen(), tea.WithContext(ctx)).Run()
	return err
}

// RunStatusTxPool launches the status-txpool alt-screen as a
// standalone bubbletea program. Same single-program contract as
// RunStateBlock: only the CLI direct-invocation path uses it; the
// unified App's menu dispatch reuses newStatusTxPoolScreen via
// a.push(...).
func RunStatusTxPool(ctx context.Context, cfg *config.Config, resolver *sshtunnel.Resolver, interval, timeout time.Duration) error {
	screen := newStatusTxPoolScreen(cfg.BackendList(), resolver, interval, timeout)
	_, err := tea.NewProgram(screen, tea.WithAltScreen(), tea.WithContext(ctx)).Run()
	return err
}

func (a App) Init() tea.Cmd {
	if len(a.stack) == 0 {
		return nil
	}
	return a.stack[0].Init()
}

func (a App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// Window size: cache + forward to every screen so the one we pop
	// back to has up-to-date dimensions even if it was off-stack
	// during a resize.
	if size, ok := msg.(tea.WindowSizeMsg); ok {
		a.width = size.Width
		a.height = size.Height
		for i, s := range a.stack {
			next, _ := s.Update(size)
			a.stack[i] = next
		}
		return a, nil
	}

	// App-level navigation / async-result messages.
	switch m := msg.(type) {
	case popMsg:
		return a.handlePop()
	case cmdSelectedMsg:
		return a.handleCmdSelected(m.name)
	case backendSelectedMsg:
		return a.handleBackendSelected(m)
	case peersFetchedMsg:
		return a.handlePeersFetched(m), nil
	case peerDetailMsg:
		return a.push(newPeerDetailScreen(m.backend, m.peerID, m.name, m.entry, m.banned)), nil
	case executionPeersFetchedMsg:
		return a.handleExecutionPeersFetched(m), nil
	case executionPeerDetailMsg:
		return a.push(newExecutionPeerDetailScreen(m.backend, m.name, m.peer)), nil
	case discoveryFetchedMsg:
		return a.handleDiscoveryFetched(m), nil
	case discoveryDetailMsg:
		return a.push(newDiscoveryDetailScreen(m.backend, m.name, m.enr)), nil
	case listLoadedMsg:
		return a.handleListLoaded(m), nil
	case txpoolDetailMsg:
		return a.handleTxpoolDetail(m)
	case txDetailMsg:
		return a.handleTxDetail(m)
	case configSelectedMsg:
		return a.handleConfigSelected(m)
	}

	if len(a.stack) == 0 {
		return a, tea.Quit
	}
	top := a.stack[len(a.stack)-1]
	next, cmd := top.Update(msg)
	a.stack[len(a.stack)-1] = next
	return a, cmd
}

func (a App) View() string {
	if len(a.stack) == 0 {
		return ""
	}
	return a.stack[len(a.stack)-1].View()
}

// push appends a screen to the stack, first feeding it the cached
// window size so its first View() renders at the right dimensions
// without waiting for the next resize event.
func (a App) push(s tea.Model) App {
	if a.width > 0 && a.height > 0 {
		next, _ := s.Update(tea.WindowSizeMsg{Width: a.width, Height: a.height})
		s = next
	}
	a.stack = append(a.stack, s)
	return a
}

// handlePop pops the top screen. If only the root remains, popping
// quits the program.
func (a App) handlePop() (App, tea.Cmd) {
	if len(a.stack) <= 1 {
		return a, tea.Quit
	}
	a.stack = a.stack[:len(a.stack)-1]
	return a, nil
}

// handleCmdSelected fires when a cmdMenu item is chosen. Parents push
// another cmdMenu; leafs route by command name to the matching flow.
// Unknown leaves no-op so an unrouted command can't deadlock the TUI.
func (a App) handleCmdSelected(name string) (App, tea.Cmd) {
	top, ok := a.stack[len(a.stack)-1].(cmdMenu)
	if !ok {
		return a, nil
	}
	var target *cobra.Command
	for _, c := range top.parent.Commands() {
		if c.Name() == name {
			target = c
			break
		}
	}
	if target == nil {
		return a, nil
	}
	if !target.Runnable() && target.HasAvailableSubCommands() {
		return a.push(newCmdMenu(target, target.Name())), nil
	}
	switch target.Name() {
	case "list":
		return a, runListCmd(a.namespaceDir)
	case "namespace":
		bs := a.cfg.BackendList()
		if len(bs) == 0 {
			return a.push(newErrScreen("no backends configured")), nil
		}
		ns, cmd := newNamespaceScreen(bs, a.resolver, a.namespaceDir, a.timeout, a.cfg.Global.NamespaceRetry)
		a = a.push(ns)
		return a, cmd
	case "consensus":
		bs := a.cfg.BackendList()
		if len(bs) == 0 {
			return a.push(newErrScreen("no backends configured")), nil
		}
		// "consensus" leaf appears under both `peer` (opp2p_peers)
		// and `discovery` (opp2p_discoveryTable) — disambiguate by
		// the immediate cobra parent.
		parent := ""
		if target.Parent() != nil {
			parent = target.Parent().Name()
		}
		switch parent {
		case "discovery":
			return a.push(newBackendMenu(bs, backendForDiscoveryConsensus)), nil
		default:
			return a.push(newBackendMenu(bs, backendForConsensus)), nil
		}
	case "execution":
		bs := a.cfg.BackendList()
		if len(bs) == 0 {
			return a.push(newErrScreen("no backends configured")), nil
		}
		return a.push(newBackendMenu(bs, backendForExecution)), nil
	case "block":
		// Disambiguate by parent: `block` only belongs under the
		// `status` parent. Other parents with a future `block` leaf
		// would have their own dispatch added here.
		if target.Parent() == nil || target.Parent().Name() != "status" {
			return a, nil
		}
		bs := a.cfg.BackendList()
		if len(bs) == 0 {
			return a.push(newErrScreen("no backends configured")), nil
		}
		interval := a.cfg.State.Block.Interval
		if interval <= 0 {
			interval = 1 * time.Second
		}
		timeout := a.cfg.State.Block.Timeout
		if timeout <= 0 {
			timeout = a.timeout
		}
		screen := newStateBlockScreen(bs, a.resolver, interval, timeout, a.cfg.Global.L2BlockTime)
		a = a.push(screen)
		return a, screen.Init()
	case "txpool":
		// Disambiguate by parent: `txpool` only belongs under `status`.
		if target.Parent() == nil || target.Parent().Name() != "status" {
			return a, nil
		}
		bs := a.cfg.BackendList()
		if len(bs) == 0 {
			return a.push(newErrScreen("no backends configured")), nil
		}
		interval := a.cfg.State.TxPool.Interval
		if interval <= 0 {
			interval = 1 * time.Second
		}
		timeout := a.cfg.State.TxPool.Timeout
		if timeout <= 0 {
			timeout = a.timeout
		}
		screen := newStatusTxPoolScreen(bs, a.resolver, interval, timeout).withAppMode()
		a = a.push(screen)
		return a, screen.Init()
	}
	return a, nil
}

func (a App) handleBackendSelected(m backendSelectedMsg) (App, tea.Cmd) {
	switch m.kind {
	case backendForConsensus:
		a = a.push(newLoadingScreen(fmt.Sprintf("opp2p_peers on %s ...", m.backend.Name)))
		return a, runPeersCmd(a.resolver, m.backend, a.timeout)
	case backendForExecution:
		a = a.push(newLoadingScreen(fmt.Sprintf("admin_peers + net_peerCount on %s ...", m.backend.Name)))
		return a, runExecutionPeersCmd(a.resolver, m.backend, a.timeout)
	case backendForDiscoveryConsensus:
		a = a.push(newLoadingScreen(fmt.Sprintf("opp2p_discoveryTable on %s ...", m.backend.Name)))
		return a, runDiscoveryCmd(a.resolver, m.backend, a.timeout)
	}
	return a, nil
}

func (a App) handlePeersFetched(m peersFetchedMsg) App {
	a = popLoading(a)
	if m.err != nil {
		return a.push(newErrScreen(m.err.Error()))
	}
	idx, ierr := namespace.LoadIndex(a.namespaceDir)
	if ierr != nil {
		return a.push(newErrScreen(ierr.Error()))
	}
	return a.push(newPeersScreen(m.backend, m.dump, idx))
}

func (a App) handleListLoaded(m listLoadedMsg) App {
	if m.err != nil {
		return a.push(newErrScreen(m.err.Error()))
	}
	return a.push(newTextScreen("list", m.text))
}

// handleExecutionPeersFetched routes the result of net_peerCount +
// admin_peers. We push the screen even when admin_peers errored so
// the operator still sees the count and a tailored "admin disabled"
// hint — only the rare "both calls failed" case promotes to a full
// error screen.
func (a App) handleExecutionPeersFetched(m executionPeersFetchedMsg) App {
	a = popLoading(a)
	if m.countErr != nil && m.peersErr != nil {
		return a.push(newErrScreen(fmt.Sprintf(
			"both execution calls failed:\nnet_peerCount: %v\nadmin_peers: %v",
			m.countErr, m.peersErr,
		)))
	}
	idx, ierr := namespace.LoadIndex(a.namespaceDir)
	if ierr != nil {
		return a.push(newErrScreen(ierr.Error()))
	}
	return a.push(newExecutionPeersScreen(m.backend, m.count, m.countErr, m.peers, m.peersErr, idx))
}

// handleDiscoveryFetched routes opp2p_discoveryTable results. We
// always push the screen — it handles both success and error paths
// (including the "discovery disabled" hint) inline so the operator
// stays inside the same flow rather than getting bounced to a
// generic error overlay.
func (a App) handleDiscoveryFetched(m discoveryFetchedMsg) App {
	a = popLoading(a)
	idx, ierr := namespace.LoadIndex(a.namespaceDir)
	if ierr != nil {
		return a.push(newErrScreen(ierr.Error()))
	}
	return a.push(newDiscoveryConsensusScreen(m.backend, m.enrs, m.err, idx))
}

// handleConfigSelected materializes cfg/resolver/timeout via the
// loader callback and swaps the picker on the stack for the root
// cobra menu. A loader error becomes an in-app errScreen rather than
// a program exit, so the operator sees the failure inside the same
// alt-screen they started in.
func (a App) handleConfigSelected(m configSelectedMsg) (App, tea.Cmd) {
	if a.loader == nil {
		// Defensive: configSelectedMsg should only arrive on the
		// picker path, which always sets loader.
		a.stack = []tea.Model{newErrScreen("internal: no config loader installed")}
		return a, nil
	}
	cfg, resolver, namespaceDir, timeout, err := a.loader(m.path)
	if err != nil {
		a.stack = []tea.Model{newErrScreen(err.Error())}
		if a.width > 0 {
			next, _ := a.stack[0].Update(tea.WindowSizeMsg{Width: a.width, Height: a.height})
			a.stack[0] = next
		}
		return a, nil
	}
	a.cfg = cfg
	a.resolver = resolver
	a.namespaceDir = namespaceDir
	a.timeout = timeout
	root := newCmdMenu(a.cobra, "op-ctl")
	if a.width > 0 {
		next, _ := root.Update(tea.WindowSizeMsg{Width: a.width, Height: a.height})
		root = next.(cmdMenu)
	}
	a.stack = []tea.Model{root}
	return a, root.Init()
}

// handleTxpoolDetail pushes the per-backend pending-tx list screen.
// Init() returns the Cmd that performs the first txpool_content fetch;
// tea routes the result back into the pushed screen on the next
// event-loop tick.
func (a App) handleTxpoolDetail(m txpoolDetailMsg) (App, tea.Cmd) {
	refresh := a.cfg.State.TxPool.Detail.Refresh
	screen := newStatusTxPoolDetailScreen(m.backend, a.resolver, refresh, a.timeout)
	a = a.push(screen)
	return a, screen.Init()
}

// handleTxDetail pushes the tx-detail screen directly. The tx pointer
// comes from the detail screen's cache (filled by txpool_content);
// no per-click RPC is performed.
func (a App) handleTxDetail(m txDetailMsg) (App, tea.Cmd) {
	return a.push(newStatusTxPoolTxDetailScreen(m.backend, m.tx)), nil
}

// popLoading removes a loadingScreen sitting at the top of the stack
// (no-op if the top isn't loading). Used by async-result handlers so
// each handler doesn't have to repeat the type-switch dance.
func popLoading(a App) App {
	if n := len(a.stack); n > 0 {
		if _, ok := a.stack[n-1].(loadingScreen); ok {
			a.stack = a.stack[:n-1]
		}
	}
	return a
}

// ---------- screen: cmdMenu ----------

// cmdMenu wraps menu.Model for a cobra command's subcommand list. It
// keeps a pointer to its cobra parent so handleCmdSelected can resolve
// the picked name back to the cobra child.
type cmdMenu struct {
	inner  menu.Model
	parent *cobra.Command
}

func newCmdMenu(parent *cobra.Command, title string) cmdMenu {
	items := []menu.Item{}
	for _, c := range parent.Commands() {
		if c.Hidden || !c.IsAvailableCommand() {
			continue
		}
		items = append(items, menu.Item{Name: c.Name(), Short: c.Short})
	}
	return cmdMenu{inner: menu.NewWithTitle(title, items), parent: parent}
}

func (s cmdMenu) Init() tea.Cmd { return s.inner.Init() }

func (s cmdMenu) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	next, cmd := s.inner.Update(msg)
	s.inner = next.(menu.Model)
	return s, translate(cmd, func(m tea.Msg) tea.Msg {
		switch v := m.(type) {
		case menu.SelectedMsg:
			return cmdSelectedMsg{name: v.Name}
		case menu.CanceledMsg:
			return popMsg{}
		}
		return m
	})
}

func (s cmdMenu) View() string { return s.inner.View() }

// ---------- screen: backendMenu ----------

// backendMenu is the same selector widget used twice — once for the
// consensus flow (URL = consensus_rpc_url) and once for execution
// (URL = execution_rpc_url). The kind field travels with the
// selection message so the app's handler picks the right RPC fan-out.
type backendMenu struct {
	inner    menu.Model
	backends []config.Backend
	kind     backendKind
}

func newBackendMenu(bs []config.Backend, kind backendKind) backendMenu {
	items := make([]menu.Item, len(bs))
	for i, b := range bs {
		short := b.ConsensusRPCURL
		if kind == backendForExecution {
			short = b.ExecutionRPCURL
		}
		items[i] = menu.Item{Name: b.Name, Short: short}
	}
	title := "p2p / peer / consensus / pick backend"
	switch kind {
	case backendForExecution:
		title = "p2p / peer / execution / pick backend"
	case backendForDiscoveryConsensus:
		title = "p2p / discovery / consensus / pick backend"
	}
	return backendMenu{
		inner:    menu.NewWithTitle(title, items),
		backends: bs,
		kind:     kind,
	}
}

func (s backendMenu) Init() tea.Cmd { return s.inner.Init() }

func (s backendMenu) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	next, cmd := s.inner.Update(msg)
	s.inner = next.(menu.Model)
	kind := s.kind
	return s, translate(cmd, func(m tea.Msg) tea.Msg {
		switch v := m.(type) {
		case menu.SelectedMsg:
			for _, b := range s.backends {
				if b.Name == v.Name {
					return backendSelectedMsg{backend: b, kind: kind}
				}
			}
			return popMsg{}
		case menu.CanceledMsg:
			return popMsg{}
		}
		return m
	})
}

func (s backendMenu) View() string { return s.inner.View() }

// ---------- screen: peersScreen ----------

// peersScreen wraps the peers package's Model. It carries the
// originating backend so when a peer row is selected we can pass that
// context into the detail-screen push.
type peersScreen struct {
	inner   peerstui.Model
	backend config.Backend
}

func newPeersScreen(b config.Backend, dump *opnode.PeerDump, idx *namespace.Index) peersScreen {
	return peersScreen{inner: peerstui.New(b, dump, idx), backend: b}
}

func (s peersScreen) Init() tea.Cmd { return s.inner.Init() }

func (s peersScreen) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	next, cmd := s.inner.Update(msg)
	s.inner = next.(peerstui.Model)
	backend := s.backend
	return s, translate(cmd, func(m tea.Msg) tea.Msg {
		switch v := m.(type) {
		case peerstui.DoneMsg:
			return popMsg{}
		case peerstui.SelectedMsg:
			return peerDetailMsg{
				backend: backend,
				peerID:  v.PeerID,
				name:    v.Name,
				entry:   v.Entry,
				banned:  v.Banned,
			}
		}
		return m
	})
}

func (s peersScreen) View() string { return s.inner.View() }

// ---------- screen: errScreen ----------

type errScreen struct{ inner errscreen.Model }

func newErrScreen(msg string) errScreen { return errScreen{inner: errscreen.New(msg)} }

func (s errScreen) Init() tea.Cmd { return s.inner.Init() }

func (s errScreen) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	next, cmd := s.inner.Update(msg)
	s.inner = next.(errscreen.Model)
	return s, translate(cmd, func(m tea.Msg) tea.Msg {
		if _, ok := m.(errscreen.DoneMsg); ok {
			return popMsg{}
		}
		return m
	})
}

func (s errScreen) View() string { return s.inner.View() }

// ---------- screen: loadingScreen ----------

type loadingScreen struct {
	title  string
	width  int
	height int
}

func newLoadingScreen(title string) loadingScreen { return loadingScreen{title: title} }

func (s loadingScreen) Init() tea.Cmd { return nil }

func (s loadingScreen) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if size, ok := msg.(tea.WindowSizeMsg); ok {
		s.width = size.Width
		s.height = size.Height
	}
	return s, nil
}

var loadingStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))

func (s loadingScreen) View() string {
	body := loadingStyle.Render("⏳ " + s.title)
	if s.width == 0 || s.height == 0 {
		return body
	}
	return lipgloss.Place(s.width, s.height, lipgloss.Center, lipgloss.Center, body)
}

// ---------- screen: textScreen ----------

// textScreen shows arbitrary text content (e.g. list output, namespace
// command log) with j/k scroll and q to dismiss.
type textScreen struct {
	title  string
	body   string
	width  int
	height int
	offset int
}

func newTextScreen(title, body string) textScreen { return textScreen{title: title, body: body} }

func (s textScreen) Init() tea.Cmd { return nil }

func (s textScreen) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch m := msg.(type) {
	case tea.WindowSizeMsg:
		s.width = m.Width
		s.height = m.Height
	case tea.KeyMsg:
		switch m.String() {
		case "q", "esc", "ctrl+c":
			return s, func() tea.Msg { return popMsg{} }
		case "j", "down":
			s.offset++
		case "k", "up":
			s.offset--
		case "g", "home":
			s.offset = 0
		case "G", "end":
			s.offset = 1 << 30
		case "pgdown", "ctrl+d", " ":
			s.offset += halfPage(s.height)
		case "pgup", "ctrl+u", "b":
			s.offset -= halfPage(s.height)
		}
	}
	return s, nil
}

var (
	textTitleStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	textHelpStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("244")).Italic(true)
)

func (s textScreen) View() string {
	header := textTitleStyle.Render(s.title) + "\n\n"
	footer := textHelpStyle.Render("j/k ↑/↓ scroll · g/G top/bottom · q back")

	if s.width == 0 || s.height == 0 {
		return header + s.body + "\n" + footer
	}

	bodyLines := strings.Split(s.body, "\n")
	avail := s.height - 3
	if avail < 1 {
		avail = 1
	}
	maxOffset := len(bodyLines) - avail
	if maxOffset < 0 {
		maxOffset = 0
	}
	off := s.offset
	if off > maxOffset {
		off = maxOffset
	}
	if off < 0 {
		off = 0
	}
	end := off + avail
	if end > len(bodyLines) {
		end = len(bodyLines)
	}
	visible := bodyLines[off:end]
	for len(visible) < avail {
		visible = append(visible, "")
	}
	return header + strings.Join(visible, "\n") + "\n" + footer
}

func halfPage(h int) int {
	if h < 4 {
		return 1
	}
	return h / 2
}

// ---------- async work ----------

// runListCmd reads the namespace dir and produces a listLoadedMsg.
// Synchronous-ish (a few file reads), but routed through tea.Cmd so
// the TUI stays responsive even on slow filesystems.
func runListCmd(dir string) tea.Cmd {
	return func() tea.Msg {
		entries, err := namespace.LoadAll(dir)
		if err != nil {
			return listLoadedMsg{err: err}
		}
		return listLoadedMsg{text: formatList(entries, dir)}
	}
}

func formatList(entries []namespace.Entry, dir string) string {
	if len(entries) == 0 {
		return fmt.Sprintf("(no entries in %s)", dir)
	}
	var b strings.Builder
	for i, e := range entries {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintln(&b, e.Name)
		fmt.Fprintln(&b, "  consensus:")
		fmt.Fprintf(&b, "    peer_id: %s\n", orEmpty(e.Consensus.PeerID))
		fmt.Fprintf(&b, "    node_id: %s\n", orEmpty(e.Consensus.NodeID))
		fmt.Fprintf(&b, "    enr:     %s\n", orEmpty(e.Consensus.ENR))
		fmt.Fprintln(&b, "  execution:")
		fmt.Fprintf(&b, "    node_id: %s\n", orEmpty(e.Execution.NodeID))
		fmt.Fprintf(&b, "    enode:   %s\n", orEmpty(e.Execution.Enode))
		fmt.Fprintf(&b, "    enr:     %s\n", orEmpty(e.Execution.ENR))
	}
	return strings.TrimRight(b.String(), "\n")
}

func orEmpty(s string) string {
	if s == "" {
		return "(empty)"
	}
	return s
}

// runPeersCmd calls opp2p_peers on the chosen backend and produces a
// peersFetchedMsg (success or error). The async result lets the
// loading screen render and animate while the RPC is in flight.
// The resolver routes through an SSH bastion when b.SSHJump is set.
func runPeersCmd(resolver *sshtunnel.Resolver, b config.Backend, timeout time.Duration) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		hc, err := resolver.HTTPClient(ctx, b.SSHJump)
		if err != nil {
			return peersFetchedMsg{backend: b, err: err}
		}
		dump, _, err := opnode.Peers(ctx, hc, b.ConsensusRPCURL, false)
		return peersFetchedMsg{backend: b, dump: dump, err: err}
	}
}

// runExecutionPeersCmd issues both net_peerCount and admin_peers in
// the same goroutine (sequential — admin_peers tends to be small,
// not worth a second goroutine) and bundles their independent
// results so the screen can render partial successes.
func runExecutionPeersCmd(resolver *sshtunnel.Resolver, b config.Backend, timeout time.Duration) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		hc, err := resolver.HTTPClient(ctx, b.SSHJump)
		if err != nil {
			return executionPeersFetchedMsg{backend: b, countErr: err, peersErr: err}
		}
		count, _, countErr := elnode.PeerCount(ctx, hc, b.ExecutionRPCURL)
		peers, _, peersErr := elnode.AdminPeers(ctx, hc, b.ExecutionRPCURL)
		return executionPeersFetchedMsg{
			backend: b, count: count, countErr: countErr,
			peers: peers, peersErr: peersErr,
		}
	}
}

// runDiscoveryCmd calls opp2p_discoveryTable and returns the result
// (including a typed RPCError when discovery is disabled — the
// screen detects that case and renders a tailored hint).
func runDiscoveryCmd(resolver *sshtunnel.Resolver, b config.Backend, timeout time.Duration) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		hc, err := resolver.HTTPClient(ctx, b.SSHJump)
		if err != nil {
			return discoveryFetchedMsg{backend: b, err: err}
		}
		enrs, _, err := opnode.DiscoveryTable(ctx, hc, b.ConsensusRPCURL)
		return discoveryFetchedMsg{backend: b, enrs: enrs, err: err}
	}
}


// ---------- helpers ----------

// translate wraps a tea.Cmd so its emitted message is mapped through
// fn before reaching the runtime. Used by screen wrappers to convert
// generic widget messages (menu.SelectedMsg, peerstui.DoneMsg...)
// into typed app-level messages (cmdSelectedMsg, popMsg...).
func translate(cmd tea.Cmd, fn func(tea.Msg) tea.Msg) tea.Cmd {
	if cmd == nil {
		return nil
	}
	return func() tea.Msg {
		return fn(cmd())
	}
}

package ui

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/huh/spinner"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/lipgloss/table"
	"github.com/mattn/go-isatty"
)

type UI struct {
	Out         io.Writer
	Err         io.Writer
	Interactive bool
}

func New(out, errOut io.Writer, isWatch bool) *UI {
	tty := false
	if f, ok := out.(*os.File); ok {
		tty = isatty.IsTerminal(f.Fd()) && isatty.IsTerminal(os.Stdin.Fd())
	}
	return &UI{
		Out:         out,
		Err:         errOut,
		Interactive: tty && !isWatch,
	}
}

var (
	styleIntro   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("205")).Padding(0, 1)
	styleInfo    = lipgloss.NewStyle().Foreground(lipgloss.Color("39"))
	styleSuccess = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	styleWarn    = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	styleErr     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("196"))
	styleMuted   = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	styleBox     = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("63")).Padding(0, 1)
	styleBoxWarn = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("214")).Padding(0, 1)
	styleTitle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("99"))
)

func (u *UI) Intro(msg string) {
	fmt.Fprintln(u.Out, styleIntro.Render(msg))
}

func (u *UI) Outro(msg string) {
	fmt.Fprintln(u.Out, styleSuccess.Render("✓ "+msg))
}

func (u *UI) Info(msg string) {
	fmt.Fprintln(u.Out, styleInfo.Render("==> "+msg))
}

func (u *UI) Note(msg string) {
	fmt.Fprintln(u.Out, styleMuted.Render(msg))
}

func (u *UI) Warn(msg string) {
	fmt.Fprintln(u.Err, styleWarn.Render("⚠ "+msg))
}

func (u *UI) Error(msg string) {
	fmt.Fprintln(u.Err, styleErr.Render("✗ igor: "+msg))
}

func (u *UI) Callout(title string, lines []string, warn bool) {
	body := styleTitle.Render(title) + "\n" + strings.Join(lines, "\n")
	if warn {
		fmt.Fprintln(u.Out, styleBoxWarn.Render(body))
		return
	}
	fmt.Fprintln(u.Out, styleBox.Render(body))
}

func (u *UI) Table(headers []string, rows [][]string) {
	t := table.New().
		Border(lipgloss.RoundedBorder()).
		BorderStyle(lipgloss.NewStyle().Foreground(lipgloss.Color("240"))).
		StyleFunc(func(row, col int) lipgloss.Style {
			if row == table.HeaderRow {
				return lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("99")).Padding(0, 1)
			}
			return lipgloss.NewStyle().Padding(0, 1)
		}).
		Headers(headers...).
		Rows(rows...)

	fmt.Fprintln(u.Out, t.String())
}

func (u *UI) Spin(title string, fn func() error) error {
	if !u.Interactive {
		u.Info(title)
		return fn()
	}
	var actionErr error
	err := spinner.New().
		Title(title).
		Action(func() {
			actionErr = fn()
		}).
		Run()
	if err != nil {
		return err
	}
	return actionErr
}

func (u *UI) Input(title, placeholder, defaultVal string) (string, error) {
	val := defaultVal
	err := huh.NewInput().
		Title(title).
		Placeholder(placeholder).
		Value(&val).
		Run()
	return strings.TrimSpace(val), err
}

func (u *UI) Select(title string, options []huh.Option[string]) (string, error) {
	var val string
	err := huh.NewSelect[string]().
		Title(title).
		Options(options...).
		Filtering(true).
		Value(&val).
		Run()
	return val, err
}

func (u *UI) Confirm(title string, defaultVal bool) (bool, error) {
	val := defaultVal
	err := huh.NewConfirm().
		Title(title).
		Value(&val).
		Run()
	return val, err
}

package workbench

import (
	"context"
	"regexp"
	"strings"
	"time"
	"unicode"
)

var cliStartupEscape = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)`)
var cliOwnedChannel = regexp.MustCompile(`Channels:server:cage(?:[❯›>]?1\.|Iamusingthisforlocaldevelopment)`)

func cliStartupText(data []byte) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, cliStartupEscape.ReplaceAllString(string(data), ""))
}

// Cage already owns this locally generated channel and the user has granted
// access to this project. Only their two startup dialogs can be completed here.
// Tool approvals, external MCP servers and login dialogs remain untouched.
func cliStartupDialog(text, workspace string) string {
	if strings.Contains(text, "WARNING:Loadingdevelopmentchannels") &&
		cliOwnedChannel.MatchString(text) &&
		strings.Contains(text, "Iamusingthisforlocaldevelopment") {
		return "channel"
	}
	if workspace != "" && strings.Contains(strings.ToLower(text), "quicksafetycheck:isthisaprojectyoucreatedoroneyoutrust?") &&
		strings.Contains(text, cliStartupText([]byte(workspace))) &&
		strings.Contains(text, "Yes,Itrustthisfolder") {
		return "project"
	}
	return ""
}

func (s *Server) prepareCLIConnection(terminal *terminalSession, id string) {
	terminal.mu.Lock()
	terminal.cliStartupDone = make(chan struct{})
	terminal.mu.Unlock()
	s.activities.Add(1)
	go func() {
		defer s.activities.Done()
		defer terminal.finishCLIStartup()
		ctx, cancel := context.WithTimeout(s.lifetimeContext(), 45*time.Second)
		defer cancel()
		s.mobileCLI.mu.Lock()
		record, err := s.mobileCLI.record(s.profile, id)
		s.mobileCLI.mu.Unlock()
		if err != nil {
			return
		}
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		seen := map[string]time.Time{}
		completed := map[string]bool{}
		nextCheck := time.Now()
		for {
			select {
			case <-ctx.Done():
				return
			case <-terminal.done:
				return
			case <-ticker.C:
			}
			if time.Now().After(nextCheck) {
				var current terminalConversation
				if err := s.runCLIChannel(ctx, "view", map[string]string{"cli_id": record.CLIID, "config": record.GuestConfig, "connection": record.Connection}, &current); err != nil {
					nextCheck = time.Now().Add(time.Second)
					continue
				}
				if current.ChannelReady || current.CanSend {
					var response struct {
						OK bool `json:"ok"`
					}
					if err := s.runCLIChannel(ctx, "rpc", map[string]string{"cli_id": record.CLIID, "connection": record.Connection, "action": "resume"}, &response); err != nil || !response.OK {
						continue
					}
					return
				}
				if current.Active || current.ConnectionNotice != "" {
					return
				}
				nextCheck = time.Now().Add(time.Second)
			}
			text := terminal.automationText()
			dialog := cliStartupDialog(text, record.GuestWorkspace)
			if dialog == "" || completed[dialog] {
				continue
			}
			if seen[dialog].IsZero() {
				seen[dialog] = time.Now()
				continue
			}
			// The CLI ignores keys during a dialog's opening guard interval.
			if time.Since(seen[dialog]) < time.Second {
				continue
			}
			input := "\r"
			if dialog == "project" && strings.Contains(text, "❯No,exit") {
				input = "\x1b[B\r"
			}
			if err := terminal.enqueueInput(ctx, input); err != nil {
				return
			}
			completed[dialog] = true
		}
	}()
}

package workbench

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
)

func (s *Server) listTerminals(w http.ResponseWriter, r *http.Request) {
	jsonReply(w, s.terminalSummaries())
}

func (s *Server) newTerminal(w http.ResponseWriter, r *http.Request) {
	var input terminalRequest
	if !readJSON(w, r, &input) {
		return
	}
	session, err := s.createTerminal(input)
	switch {
	case errors.Is(err, errTerminalsClosed):
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
	case errors.Is(err, errTerminalBusy), errors.Is(err, errTerminalNotReady), errors.Is(err, errTerminalLimit):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, errTerminalStart):
		http.Error(w, err.Error(), http.StatusInternalServerError)
	case err != nil:
		requestError(w, err)
	default:
		jsonReply(w, session.summary())
	}
}

func (s *Server) closeTerminal(w http.ResponseWriter, r *http.Request) {
	if terminal := s.terminals.get(r.PathValue("id")); terminal != nil {
		if id, _ := terminal.summary()["cli_session_id"].(string); id != "" {
			if err := s.setSessionArchived(id, true, nil); err != nil {
				requestError(w, err)
				return
			}
			jsonReply(w, map[string]bool{"ok": true})
			return
		}
	}
	s.terminals.remove(r.PathValue("id"))
	jsonReply(w, map[string]bool{"ok": true})
}

func (s *Server) terminalStream(w http.ResponseWriter, r *http.Request) {
	session := s.terminals.get(r.PathValue("id"))
	if session == nil {
		http.NotFound(w, r)
		return
	}
	ws, err := s.upgrade(w, r)
	if err != nil {
		return
	}
	defer ws.Close()
	ws.SetReadLimit(1 << 20)
	subscription := session.subscribe()
	defer subscription.Close()
	if subscription.CLIStartup != nil {
		_ = ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if err := ws.WriteJSON(map[string]any{"type": "cli-startup", "pending": subscription.CLIStarting}); err != nil {
			return
		}
	}
	if subscription.Lost {
		_ = ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if err := ws.WriteJSON(map[string]any{"type": "lost"}); err != nil {
			return
		}
	}
	if subscription.InitialGap != nil {
		_ = ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if err := ws.WriteJSON(map[string]any{"type": "gap", "from": subscription.InitialGap.From, "to": subscription.InitialGap.To, "omitted": subscription.InitialGap.Omitted}); err != nil {
			return
		}
	}
	if len(subscription.History) > 0 {
		_ = ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
		// Replayed output must render without repeating clipboard side effects.
		if err := ws.WriteJSON(map[string]string{"type": "history"}); err != nil {
			return
		}
		if err := ws.WriteMessage(websocket.BinaryMessage, subscription.History); err != nil {
			return
		}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer ws.Close()
		startup := subscription.CLIStartup
		if !subscription.CLIStarting {
			startup = nil
		}
		for {
			select {
			case <-startup:
				startup = nil
				_ = ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
				if ws.WriteJSON(map[string]any{"type": "cli-startup", "pending": false}) != nil {
					return
				}
			case chunk, ok := <-subscription.Output:
				if !ok {
					return
				}
				_ = ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
				if chunk.InputStatus != "" {
					if ws.WriteJSON(map[string]any{"type": "input-status", "id": chunk.InputID, "status": chunk.InputStatus}) != nil {
						return
					}
					continue
				}
				if chunk.Replay {
					if ws.WriteJSON(map[string]string{"type": "history"}) != nil {
						return
					}
				}
				if len(chunk.Data) > 0 && ws.WriteMessage(websocket.BinaryMessage, chunk.Data) != nil {
					return
				}
			case gap, ok := <-subscription.Gaps:
				if !ok {
					return
				}
				_ = ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
				if ws.WriteJSON(map[string]any{"type": "gap", "from": gap.From, "to": gap.To, "omitted": gap.Omitted}) != nil {
					return
				}
			}
		}
	}()
	defer func() {
		subscription.Close()
		_ = ws.Close()
		<-done
	}()
	for {
		_, data, err := ws.ReadMessage()
		if err != nil {
			return
		}
		var input struct {
			Type string `json:"type"`
			ID   string `json:"id"`
			Data string `json:"data"`
			Cols uint16 `json:"cols"`
			Rows uint16 `json:"rows"`
		}
		if json.Unmarshal(data, &input) != nil {
			continue
		}
		switch input.Type {
		case "input":
			if err := session.enqueueInputID(r.Context(), input.ID, input.Data); err != nil {
				return
			}
		case "resize":
			if input.Cols > 1 && input.Cols < 600 && input.Rows > 1 && input.Rows < 400 {
				if err := session.file.Resize(input.Cols, input.Rows); err != nil {
					return
				}
			}
		}
	}
}

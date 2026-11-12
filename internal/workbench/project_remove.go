package workbench

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
)

func (s *Server) removeProject(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Workspace string `json:"workspace"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	if !s.op.TryLock() {
		http.Error(w, "另一个操作仍在进行，请稍候", http.StatusConflict)
		return
	}
	defer s.op.Unlock()
	s.mu.Lock()
	closing, ready, preferences := s.closing, s.ready, s.preferences
	s.mu.Unlock()
	if closing || s.busy() {
		http.Error(w, "环境正在处理其他操作，请稍后重试", http.StatusConflict)
		return
	}
	path := input.Workspace
	if path == "" || path == s.environmentDir() || isClientMountPath(path) {
		http.Error(w, "请选择已添加的项目。默认工作区保留，客户端共享请使用停止共享。", http.StatusBadRequest)
		return
	}
	projects, err := s.projectPaths()
	if err != nil {
		requestError(w, err)
		return
	}
	if !slices.Contains(projects, path) {
		jsonReply(w, map[string]string{"workspace": s.workspace()})
		return
	}
	if !ready {
		running, err := controllerVMState(s.profile)
		if err != nil || running {
			http.Error(w, "环境仍在运行或运行状态无法确认。请恢复连接，或保存工作并停止环境后再移除项目。", http.StatusConflict)
			return
		}
	}
	for _, terminal := range s.terminals.snapshot() {
		if terminal["workspace"] == path && terminal["closed"] != true {
			http.Error(w, "请先关闭此项目的终端，再移除项目。会话历史会保留。", http.StatusConflict)
			return
		}
	}
	if ready && sharedCLILauncher(claudeTerminalCommand(preferences)) {
		granted := make(map[string]bool, len(projects))
		for _, project := range projects {
			granted[project] = true
		}
		_, err = s.mobileCLI.summaries(s.profile, granted, false)
		if err == nil {
			err = s.discoverTerminalConversations(r.Context(), projects)
		}
		if err != nil {
			http.Error(w, "暂时无法核对运行中的会话。请恢复会话连接，或保存工作并停止环境后再移除项目。", http.StatusConflict)
			return
		}
	}
	s.mobileCLI.mu.Lock()
	active := false
	for _, run := range s.mobileCLI.active {
		active = active || run.record.Workspace == path
	}
	for id, running := range s.mobileCLI.running {
		active = active || ready && running && s.mobileCLI.catalog[id].Workspace == path
	}
	s.mobileCLI.mu.Unlock()
	if active {
		http.Error(w, "请先结束此项目正在运行的会话，再移除项目。会话历史会保留。", http.StatusConflict)
		return
	}
	if ready {
		body, err := s.browserAction(r.Context(), "tabs", nil)
		var browser struct {
			Tabs []struct {
				Kind      string `json:"kind"`
				Workspace string `json:"workspace"`
			} `json:"tabs"`
		}
		if err != nil || json.Unmarshal(body, &browser) != nil || browser.Tabs == nil {
			http.Error(w, "暂时无法核对文件编辑器。请恢复浏览器连接，或保存工作并停止环境后再移除项目。", http.StatusConflict)
			return
		}
		for _, tab := range browser.Tabs {
			if tab.Kind == "editor" && tab.Workspace == path {
				http.Error(w, "请先保存文件并关闭此项目的编辑器，再移除项目。", http.StatusConflict)
				return
			}
		}
	}
	s.projectRecordsMu.Lock()
	saved, err := s.loadProjectRecordsLocked()
	if err != nil && !os.IsNotExist(err) {
		s.projectRecordsMu.Unlock()
		requestError(w, err)
		return
	}
	if preferences.Workspace == path {
		// Preserve a legacy current grant before changing the selection. If a
		// later write fails, a retry can still find and remove this project.
		if !slices.Contains(saved.Paths, path) {
			saved.Paths = append(saved.Paths, path)
			if err = s.writeProjectRecordsLocked(saved); err != nil {
				s.projectRecordsMu.Unlock()
				requestError(w, err)
				return
			}
		}
		preferences.Workspace = s.environmentDir()
		if err = writePrivate(filepath.Join(s.profile, "workbench.json"), preferences); err != nil {
			s.projectRecordsMu.Unlock()
			requestError(w, err)
			return
		}
		s.mu.Lock()
		s.preferences.Workspace = preferences.Workspace
		s.mu.Unlock()
	}
	deleteProject := func(value string) bool { return value == path }
	saved.Paths = slices.DeleteFunc(saved.Paths, deleteProject)
	if err = s.writeProjectRecordsLocked(saved); err != nil {
		s.projectRecordsMu.Unlock()
		requestError(w, err)
		return
	}
	s.mu.Lock()
	share := s.sealProjectShareLocked(path, projectShareRevoked)
	s.mu.Unlock()
	s.projectRecordsMu.Unlock()
	if share != nil {
		if share.cancel != nil {
			share.cancel()
		}
		share.close()
	}
	s.setProjectError(path, nil)
	jsonReply(w, map[string]string{"workspace": s.workspace()})
}

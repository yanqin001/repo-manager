package manager

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

// Handler 返回同步任务的 HTTP 路由。服务部署在调用方可信的内网，无应用层鉴权。
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/sync", s.handleSubmit)
	mux.HandleFunc("GET /v1/tasks/{id}", s.handleTask)
	return mux
}

func (s *Service) handleSubmit(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL string `json:"url"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil || decoder.Decode(new(any)) != io.EOF {
		jsonError(w, http.StatusBadRequest, "请求必须是包含 url 字段的 JSON")
		return
	}
	repo, err := parseRepository(req.URL)
	if err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	task, err := s.submit(repo)
	if errors.Is(err, errBusy) {
		jsonError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, task)
}

func (s *Service) handleTask(w http.ResponseWriter, r *http.Request) {
	task, ok := s.get(r.PathValue("id"))
	if !ok {
		jsonError(w, http.StatusNotFound, "同步任务不存在或已过期")
		return
	}
	writeJSON(w, http.StatusOK, task)
}

func jsonError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

package api

import "net/http"

func (s *Server) cancel(w http.ResponseWriter, r *http.Request) {
	res, err := s.db.Cancel(r.Context(), r.PathValue("id"), userID(r))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"reservation_id": res.ID,
		"status":         res.Status,
		"released":       res.Seats,
	})
}

func (s *Server) confirm(w http.ResponseWriter, r *http.Request) {
	res, err := s.db.Confirm(r.Context(), r.PathValue("id"), userID(r))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

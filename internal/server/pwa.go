package server

import (
	"net/http"

	"github.com/Saransh-Jainbu/nestnas/internal/web"
)

// Public, static assets that let a phone install the dashboard as an app ("Add to Home Screen").

func (s *Server) manifest(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/manifest+json")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write([]byte(`{"name":"NestNAS","short_name":"NestNAS","description":"Your own private cloud storage","start_url":"/","scope":"/","display":"standalone","background_color":"#05080c","theme_color":"#05080c","icons":[{"src":"/icon.png","sizes":"256x256","type":"image/png","purpose":"any"},{"src":"/icon-512.png","sizes":"512x512","type":"image/png","purpose":"any"}]}`))
}

func (s *Server) icon(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(web.Icon)
}

func (s *Server) icon512(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(web.Icon512)
}

func (s *Server) favicon(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(web.Favicon)
}

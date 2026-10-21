package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func TestUIServesTheAppAndItsRoutes(t *testing.T) {
	h := uiHandler(fstest.MapFS{
		"webui/dist/index.html":        {Data: []byte(`<div id="root"></div>`)},
		"webui/dist/assets/app-1a2.js": {Data: []byte(`console.log(1)`)},
	})
	for _, c := range []struct{ path, want, cache string }{
		{"/ui/", `<div id="root">`, "no-store"},
		{"/ui/settings", `<div id="root">`, "no-store"}, // the app's own route
		{"/ui/assets/app-1a2.js", "console.log", "immutable"},
		{"/ui/assets/", `<div id="root">`, "no-store"}, // never a directory listing
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, c.path, nil))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), c.want) || !strings.Contains(rec.Header().Get("Cache-Control"), c.cache) {
			t.Errorf("%s: %d %q cache=%q", c.path, rec.Code, rec.Body.String(), rec.Header().Get("Cache-Control"))
		}
	}

	rec := httptest.NewRecorder()
	uiHandler(fstest.MapFS{"webui/README.md": {}}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ui/", nil))
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "make web") {
		t.Errorf("without a built UI: %d %q", rec.Code, rec.Body.String())
	}
}

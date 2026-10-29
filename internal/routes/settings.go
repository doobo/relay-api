package routes

import (
	"net/http"

	"relay-api/internal/db"
	"relay-api/internal/httperr"
	"relay-api/internal/ipacl"
)

// maxIPAccessEntries keeps a mistaken paste from turning every request into a
// long linear scan. Real allow/deny lists are far smaller.
const maxIPAccessEntries = 4096

// SettingsRoutes serves the gateway-wide settings that are not tied to a
// resource row. The parent /admin subtree authenticates the caller and its
// RequireAdminRole wrapper makes GET read-only and everything else admin-only.
type SettingsRoutes struct {
	ipAccess *ipacl.Store
}

// NewSettingsRoutes builds the settings handlers. ipAccess is updated in place
// when a save succeeds, so the middleware sees the new rules immediately.
func NewSettingsRoutes(ipAccess *ipacl.Store) *SettingsRoutes {
	return &SettingsRoutes{ipAccess: ipAccess}
}

// Register mounts the settings routes.
func (h *SettingsRoutes) Register(mux *http.ServeMux) {
	mux.HandleFunc("/admin/settings/ip-access", h.ipAccessSettings)
}

type ipAccessSettingsDTO struct {
	Whitelist []string `json:"whitelist"`
	Blacklist []string `json:"blacklist"`
	UpdatedAt int64    `json:"updated_at"`
}

func (h *SettingsRoutes) ipAccessSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		settings, err := db.GetIPAccessSettings()
		if err != nil {
			internalError(w, "get IP access settings", err)
			return
		}
		httperr.WriteJSON(w, http.StatusOK, toIPAccessSettingsDTO(settings))
	case http.MethodPut:
		h.updateIPAccessSettings(w, r)
	default:
		methodNotAllowed(w, "GET, PUT")
	}
}

func (h *SettingsRoutes) updateIPAccessSettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Whitelist []string `json:"whitelist"`
		Blacklist []string `json:"blacklist"`
	}
	if _, ok := decodeBody(w, r, &body); !ok {
		return
	}
	if len(body.Whitelist) > maxIPAccessEntries || len(body.Blacklist) > maxIPAccessEntries {
		invalidRequest(w, "Too many IP access entries")
		return
	}

	// Validate before touching the database, so a typo cannot leave the
	// persisted rules and the live middleware disagreeing.
	whitelist, err := ipacl.Normalize(body.Whitelist)
	if err != nil {
		invalidRequest(w, err.Error())
		return
	}
	blacklist, err := ipacl.Normalize(body.Blacklist)
	if err != nil {
		invalidRequest(w, err.Error())
		return
	}
	rules, err := ipacl.Compile(whitelist, blacklist)
	if err != nil {
		invalidRequest(w, err.Error())
		return
	}

	settings, err := db.SaveIPAccessSettings(whitelist, blacklist)
	if err != nil {
		internalError(w, "save IP access settings", err)
		return
	}
	h.ipAccess.Store(rules)
	httperr.WriteJSON(w, http.StatusOK, toIPAccessSettingsDTO(settings))
}

func toIPAccessSettingsDTO(settings db.IPAccessSettings) ipAccessSettingsDTO {
	whitelist := settings.Whitelist
	if whitelist == nil {
		whitelist = []string{}
	}
	blacklist := settings.Blacklist
	if blacklist == nil {
		blacklist = []string{}
	}
	return ipAccessSettingsDTO{
		Whitelist: whitelist,
		Blacklist: blacklist,
		UpdatedAt: settings.UpdatedAt,
	}
}

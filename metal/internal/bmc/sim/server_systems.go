package sim

import (
	"maps"
	"net/http"
	"time"

	"github.com/dennisklein/platform-kit/metal/internal/bmc"
)

// ComputerSystem and what hangs off it: host NICs, storage, BIOS.

func (s *Server) systemRoutes() {
	s.mux.HandleFunc("GET "+systemsURI, s.systems)
	s.mux.HandleFunc("GET "+systemsURI+"/{id}", s.system)
	s.mux.HandleFunc("PATCH "+systemsURI+"/{id}", s.patchSystem)
	s.mux.HandleFunc("POST "+systemsURI+"/{id}/Actions/ComputerSystem.Reset", s.reset)
	s.mux.HandleFunc("GET "+systemsURI+"/{id}/EthernetInterfaces", s.systemNICs)
	s.mux.HandleFunc("GET "+systemsURI+"/{id}/EthernetInterfaces/{n}", s.systemNIC)
	if s.f.Layout.Drives == layoutStorage {
		s.mux.HandleFunc("GET "+systemsURI+"/{id}/Storage", s.storageCollection)
		s.mux.HandleFunc("GET "+systemsURI+"/{id}/Storage/1", s.storage)
		s.mux.HandleFunc("GET "+systemsURI+"/{id}/Storage/1/Drives/{n}", s.storageDrive)
	}
	if s.f.Bios.Attributes != nil {
		s.mux.HandleFunc("GET "+systemsURI+"/{id}/Bios", s.bios)
		s.mux.HandleFunc("GET "+systemsURI+"/{id}/Bios/{setting}", s.biosSettings)
		s.mux.HandleFunc("PATCH "+systemsURI+"/{id}/Bios/{setting}", s.patchBiosSettings)
	}
}

// systemOf is the live view of the system that the {id} path value names.
func (s *Server) systemOf(r *http.Request) (*bmc.System, error) {
	systems, err := s.st.Systems(r.Context())
	if err != nil {
		return nil, err
	}
	id := r.PathValue("id")
	for i := range systems {
		if systems[i].ID == id {
			return &systems[i], nil
		}
	}
	return nil, notFound("ComputerSystem", id)
}

func (s *Server) systems(w http.ResponseWriter, r *http.Request) {
	systems, err := s.st.Systems(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	s.reply(w, r, collection("ComputerSystem", systemsURI, memberIDs(systems, func(sys bmc.System) string { return sys.ID })))
}

func (s *Server) system(w http.ResponseWriter, r *http.Request) {
	sys, err := s.systemOf(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	fs := s.f.system(sys.ID)
	self := systemsURI + "/" + sys.ID
	doc := resource("#ComputerSystem.v1_23_1.ComputerSystem", self, sys.ID, "System")
	doc["SystemType"] = "Physical"
	doc["UUID"] = sys.UUID
	doc["SerialNumber"] = sys.SerialNumber
	doc["Manufacturer"] = sys.Manufacturer
	doc["Model"] = sys.Model
	doc["PowerState"] = string(sys.PowerState)
	doc["Status"] = map[string]string{"State": "Enabled", "Health": "OK"}
	doc["MemorySummary"] = map[string]any{"TotalSystemMemoryGiB": sys.MemoryGiB}
	doc["ProcessorSummary"] = map[string]any{"Count": sys.CPUCount, "Model": sys.CPUModel}
	if fs.SKU != "" {
		doc["SKU"] = fs.SKU
	}

	enabled, target, mode := s.st.bootView(sys.ID)
	boot := map[string]any{
		"BootSourceOverrideEnabled":                        enabled,
		"BootSourceOverrideTarget":                         target,
		"BootSourceOverrideTarget@Redfish.AllowableValues": sys.BootTargets,
	}
	if mode != "" {
		boot["BootSourceOverrideMode"] = mode
	}
	doc["Boot"] = boot
	// Served only where the fixture says so, like every observable a vendor
	// may withhold.
	if sys.BootProgress != "" {
		doc["BootProgress"] = map[string]any{"LastState": sys.BootProgress}
	}
	if !sys.LastResetTime.IsZero() {
		doc["LastResetTime"] = sys.LastResetTime.Format(time.RFC3339Nano)
	}

	doc["EthernetInterfaces"] = link(self + "/EthernetInterfaces")
	if s.f.Bios.Attributes != nil {
		doc["Bios"] = link(self + "/Bios")
	}
	if s.f.Layout.Drives == layoutStorage {
		doc["Storage"] = link(self + "/Storage")
	}
	if s.f.Layout.VirtualMedia == layoutSystem {
		doc["VirtualMedia"] = link(s.f.mediaBase())
	}
	doc["Links"] = map[string]any{
		"ManagedBy": links(managerURI),
		"Chassis":   links(chassisURI),
	}
	doc["Actions"] = map[string]any{
		"#ComputerSystem.Reset": map[string]any{
			"target":                            self + "/Actions/ComputerSystem.Reset",
			"ResetType@Redfish.AllowableValues": fs.resetTypes(),
		},
	}
	s.reply(w, r, doc)
}

// patchSystem updates the Boot object — the only writable part of a system
// this sim models. Properties are independent: a request names the ones it
// changes.
func (s *Server) patchSystem(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Boot *struct {
			Target  *string `json:"BootSourceOverrideTarget"`
			Enabled *string `json:"BootSourceOverrideEnabled"`
			Mode    *string `json:"BootSourceOverrideMode"`
		} `json:"Boot"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	if body.Boot == nil || (body.Boot.Target == nil && body.Boot.Enabled == nil && body.Boot.Mode == nil) {
		s.fail(w, refuse("the request changes no property this resource can update (Boot)"))
		return
	}
	s.noContent(w, s.st.patchBoot(r.PathValue("id"), body.Boot.Enabled, body.Boot.Target, body.Boot.Mode))
}

func (s *Server) reset(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ResetType string `json:"ResetType"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	if body.ResetType == "" {
		s.fail(w, missing("ComputerSystem.Reset", "ResetType"))
		return
	}
	s.noContent(w, s.st.SetPower(r.Context(), r.PathValue("id"), bmc.PowerAction(body.ResetType)))
}

func (s *Server) systemNICs(w http.ResponseWriter, r *http.Request) {
	sys, err := s.systemOf(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.reply(w, r, collection("EthernetInterface", systemsURI+"/"+sys.ID+"/EthernetInterfaces", ordinals(len(sys.NICs))))
}

func (s *Server) systemNIC(w http.ResponseWriter, r *http.Request) {
	sys, err := s.systemOf(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	n := r.PathValue("n")
	nic, err := nth(sys.NICs, "EthernetInterface", n)
	if err != nil {
		s.fail(w, err)
		return
	}
	uri := systemsURI + "/" + sys.ID + "/EthernetInterfaces/" + n
	doc := resource("#EthernetInterface.v1_12_3.EthernetInterface", uri, nic.ID, "Ethernet Interface")
	doc["MACAddress"] = nic.MAC
	doc["PermanentMACAddress"] = nic.MAC
	doc["InterfaceEnabled"] = true
	s.reply(w, r, doc)
}

func (s *Server) storageCollection(w http.ResponseWriter, r *http.Request) {
	sys, err := s.systemOf(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.reply(w, r, collection("Storage", systemsURI+"/"+sys.ID+"/Storage", []string{"1"}))
}

func (s *Server) storage(w http.ResponseWriter, r *http.Request) {
	sys, err := s.systemOf(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	self := systemsURI + "/" + sys.ID + "/Storage/1"
	doc := resource("#Storage.v1_17_1.Storage", self, "1", "Storage")
	doc["Drives"] = memberLinks(self+"/Drives", ordinals(len(sys.Drives)))
	doc["Drives@odata.count"] = len(sys.Drives)
	doc["Status"] = map[string]string{"State": "Enabled", "Health": "OK"}
	s.reply(w, r, doc)
}

func (s *Server) storageDrive(w http.ResponseWriter, r *http.Request) {
	sys, err := s.systemOf(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.serveDrive(w, r, sys, systemsURI+"/"+sys.ID+"/Storage/1/Drives")
}

// serveDrive renders one Drive under base — the same resource whether the
// fixture hangs drives off the chassis or off a storage subsystem.
func (s *Server) serveDrive(w http.ResponseWriter, r *http.Request, sys *bmc.System, base string) {
	n := r.PathValue("n")
	d, err := nth(sys.Drives, "Drive", n)
	if err != nil {
		s.fail(w, err)
		return
	}
	doc := resource("#Drive.v1_20_1.Drive", base+"/"+n, n, "Drive")
	doc["Model"] = d.Model
	doc["SerialNumber"] = d.Serial
	doc["CapacityBytes"] = int64(d.CapacityGiB) << 30
	// Served only when the fixture carries them — an endpoint that omits the
	// property must keep omitting it, so the fact-file layer's "?" convention
	// gets exercised.
	if d.MediaType != "" {
		doc["MediaType"] = d.MediaType
	}
	if d.Protocol != "" {
		doc["Protocol"] = d.Protocol
	}
	s.reply(w, r, doc)
}

func (s *Server) bios(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	current, _, messages, err := s.st.biosView(id)
	if err != nil {
		s.fail(w, err)
		return
	}
	self := systemsURI + "/" + id + "/Bios"
	settings := map[string]any{
		"@odata.type":         "#Settings.v1_4_0.Settings",
		"SettingsObject":      link(self + "/" + s.f.Bios.SettingsID),
		"SupportedApplyTimes": s.st.supportedApplyTimes(),
	}
	if len(messages) > 0 {
		msgs := make([]map[string]string, 0, len(messages))
		for _, m := range messages {
			msgs = append(msgs, map[string]string{"Message": m})
		}
		settings["Messages"] = msgs
	}
	doc := resource("#Bios.v1_2_3.Bios", self, "Bios", "BIOS Configuration Current Settings")
	doc["AttributeRegistry"] = s.f.Bios.Registry
	doc["@Redfish.Settings"] = settings
	doc["Attributes"] = current
	s.reply(w, r, doc)
}

func (s *Server) biosSettings(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if r.PathValue("setting") != s.f.Bios.SettingsID {
		s.fail(w, notFound("Bios", r.PathValue("setting")))
		return
	}
	current, pending, _, err := s.st.biosView(id)
	if err != nil {
		s.fail(w, err)
		return
	}
	uri := systemsURI + "/" + id + "/Bios/" + s.f.Bios.SettingsID
	doc := resource("#Bios.v1_2_3.Bios", uri, s.f.Bios.SettingsID, "BIOS Configuration Pending Settings")
	doc["AttributeRegistry"] = s.f.Bios.Registry
	// The pending object carries no Attributes key at all while nothing is
	// staged — the shape both captured vendors serve, and a legal one. The
	// iLO shape serves every attribute, staged values merged in.
	if s.f.Bios.PendingFullSet {
		maps.Copy(current, pending)
		pending = current
	}
	if len(pending) > 0 {
		doc["Attributes"] = pending
	}
	s.reply(w, r, doc)
}

func (s *Server) patchBiosSettings(w http.ResponseWriter, r *http.Request) {
	if r.PathValue("setting") != s.f.Bios.SettingsID {
		s.fail(w, notFound("Bios", r.PathValue("setting")))
		return
	}
	var body struct {
		Attributes map[string]any `json:"Attributes"`
		ApplyTime  struct {
			ApplyTime string `json:"ApplyTime"`
		} `json:"@Redfish.SettingsApplyTime"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	if len(body.Attributes) == 0 {
		s.fail(w, refuse("the request changes no property this resource can update (Attributes)"))
		return
	}
	s.noContent(w, s.st.stageBios(r.Context(), r.PathValue("id"), body.Attributes, body.ApplyTime.ApplyTime))
}

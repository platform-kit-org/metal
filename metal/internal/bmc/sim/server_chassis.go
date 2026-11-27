package sim

import (
	"net/http"

	"github.com/dennisklein/platform-kit/metal/internal/bmc"
)

// The Chassis: the board serial, the network adapters as hardware (the host
// NICs seen from the chassis side), and — on fixtures that keep them there —
// the drives.

func (s *Server) chassisRoutes() {
	s.mux.HandleFunc("GET "+rootURI+"/Chassis", s.chassisCollection)
	s.mux.HandleFunc("GET "+chassisURI, s.chassis)
	s.mux.HandleFunc("GET "+chassisURI+"/NetworkAdapters", s.adapters)
	s.mux.HandleFunc("GET "+chassisURI+"/NetworkAdapters/{n}", s.adapter)
	switch s.f.Layout.AdapterMACs {
	case layoutDeviceFunctions:
		s.mux.HandleFunc("GET "+chassisURI+"/NetworkAdapters/{n}/NetworkDeviceFunctions", s.deviceFunctions)
		s.mux.HandleFunc("GET "+chassisURI+"/NetworkAdapters/{n}/NetworkDeviceFunctions/{p}", s.deviceFunction)
	case layoutNetworkPorts:
		s.mux.HandleFunc("GET "+chassisURI+"/NetworkAdapters/{n}/NetworkPorts", s.networkPorts)
		s.mux.HandleFunc("GET "+chassisURI+"/NetworkAdapters/{n}/NetworkPorts/{p}", s.networkPort)
	}
	if s.f.Layout.Drives != layoutStorage {
		s.mux.HandleFunc("GET "+chassisURI+"/Drives/{n}", s.chassisDrive)
	}
}

func (s *Server) chassisCollection(w http.ResponseWriter, r *http.Request) {
	s.reply(w, r, collection("Chassis", rootURI+"/Chassis", []string{"1"}))
}

func (s *Server) chassis(w http.ResponseWriter, r *http.Request) {
	c, err := s.st.Chassis(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	systems, err := s.st.Systems(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	doc := resource("#Chassis.v1_26_0.Chassis", chassisURI, "1", "Chassis")
	doc["ChassisType"] = "RackMount"
	doc["Manufacturer"] = s.f.Vendor
	doc["Model"] = c.Model
	doc["PowerState"] = string(systems[0].PowerState)
	doc["Status"] = map[string]string{"State": "Enabled", "Health": "OK"}
	doc["NetworkAdapters"] = link(chassisURI + "/NetworkAdapters")
	clinks := map[string]any{
		"ComputerSystems": links(systemsURI + "/" + systems[0].ID),
		"ManagedBy":       links(managerURI),
	}
	if s.f.Layout.Drives != layoutStorage {
		clinks["Drives"] = memberLinks(chassisURI+"/Drives", ordinals(len(systems[0].Drives)))
	}
	doc["Links"] = clinks
	// The X12 leaves SerialNumber empty and carries the board serial in its
	// OEM block; everyone else serves it where the schema puts it.
	if s.f.Chassis.OemBoardSerial {
		doc["SerialNumber"] = ""
		doc["Oem"] = map[string]any{s.f.Vendor: map[string]any{"BoardSerialNumber": c.BoardSerial}}
	} else {
		doc["SerialNumber"] = c.BoardSerial
	}
	s.reply(w, r, doc)
}

func (s *Server) chassisDrive(w http.ResponseWriter, r *http.Request) {
	systems, err := s.st.Systems(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	s.serveDrive(w, r, &systems[0], chassisURI+"/Drives")
}

func (s *Server) adapters(w http.ResponseWriter, r *http.Request) {
	c, err := s.st.Chassis(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	s.reply(w, r, collection("NetworkAdapter", chassisURI+"/NetworkAdapters", ordinals(len(c.Adapters))))
}

// adapterOf is the adapter that the {n} path value names, and its URI.
func (s *Server) adapterOf(r *http.Request) (bmc.Adapter, string, error) {
	c, err := s.st.Chassis(r.Context())
	if err != nil {
		return bmc.Adapter{}, "", err
	}
	a, err := nth(c.Adapters, "NetworkAdapter", r.PathValue("n"))
	return a, chassisURI + "/NetworkAdapters/" + r.PathValue("n"), err
}

func (s *Server) adapter(w http.ResponseWriter, r *http.Request) {
	a, self, err := s.adapterOf(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	doc := resource("#NetworkAdapter.v1_11_0.NetworkAdapter", self, r.PathValue("n"), "Network Adapter")
	doc["Model"] = a.Model
	switch s.f.Layout.AdapterMACs {
	case layoutDeviceFunctions:
		doc["NetworkDeviceFunctions"] = link(self + "/NetworkDeviceFunctions")
	case layoutNetworkPorts:
		doc["NetworkPorts"] = link(self + "/NetworkPorts")
	}
	s.reply(w, r, doc)
}

func (s *Server) deviceFunctions(w http.ResponseWriter, r *http.Request) {
	a, self, err := s.adapterOf(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.reply(w, r, collection("NetworkDeviceFunction", self+"/NetworkDeviceFunctions", ordinals(len(a.PortMACs))))
}

// deviceFunction is where the specification puts a port's MAC today: the
// Ethernet block of the function the host sees.
func (s *Server) deviceFunction(w http.ResponseWriter, r *http.Request) {
	a, self, err := s.adapterOf(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	p := r.PathValue("p")
	mac, err := nth(a.PortMACs, "NetworkDeviceFunction", p)
	if err != nil {
		s.fail(w, err)
		return
	}
	doc := resource("#NetworkDeviceFunction.v1_9_2.NetworkDeviceFunction", self+"/NetworkDeviceFunctions/"+p, p, "Network Device Function")
	doc["NetDevFuncType"] = "Ethernet"
	doc["DeviceEnabled"] = true
	doc["Ethernet"] = map[string]any{"MACAddress": mac, "PermanentMACAddress": mac}
	s.reply(w, r, doc)
}

func (s *Server) networkPorts(w http.ResponseWriter, r *http.Request) {
	a, self, err := s.adapterOf(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.reply(w, r, collection("NetworkPort", self+"/NetworkPorts", ordinals(len(a.PortMACs))))
}

// networkPort is the deprecated NetworkPort resource the iBMC still serves;
// the MAC sits in AssociatedNetworkAddresses.
func (s *Server) networkPort(w http.ResponseWriter, r *http.Request) {
	a, self, err := s.adapterOf(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	p := r.PathValue("p")
	mac, err := nth(a.PortMACs, "NetworkPort", p)
	if err != nil {
		s.fail(w, err)
		return
	}
	doc := resource("#NetworkPort.v1_4_3.NetworkPort", self+"/NetworkPorts/"+p, p, p)
	doc["PhysicalPortNumber"] = p
	doc["AssociatedNetworkAddresses"] = []string{mac}
	s.reply(w, r, doc)
}

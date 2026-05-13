package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"

	"github.com/grandcat/zeroconf"
)

type oscquery struct {
	oscConn      net.PacketConn
	httpListener net.Listener
	httpServer   *http.Server
	mdnsOSC      *zeroconf.Server
	mdnsJSON     *zeroconf.Server
}

func (q *oscquery) OSCPort() int  { return q.oscConn.LocalAddr().(*net.UDPAddr).Port }
func (q *oscquery) HTTPPort() int { return q.httpListener.Addr().(*net.TCPAddr).Port }

func startOSCQuery(name string) (*oscquery, error) {
	oscConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("osc listen: %w", err)
	}
	oscPort := oscConn.LocalAddr().(*net.UDPAddr).Port

	httpL, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		oscConn.Close()
		return nil, fmt.Errorf("http listen: %w", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if _, ok := r.URL.Query()["HOST_INFO"]; ok {
			_ = json.NewEncoder(w).Encode(hostInfo{
				Name:         name,
				OSCIP:        "127.0.0.1",
				OSCPort:      oscPort,
				OSCTransport: "UDP",
				Extensions: map[string]bool{
					"ACCESS":      true,
					"VALUE":       true,
					"DESCRIPTION": true,
				},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(rootNode())
	})

	srv := &http.Server{Handler: mux}
	go srv.Serve(httpL)

	mdnsOSC, err := zeroconf.RegisterProxy(name, "_osc._udp", "local.", oscPort, "localhost.local.", []string{"127.0.0.1"}, []string{"txtvers=1"}, nil)
	if err != nil {
		srv.Shutdown(context.Background())
		oscConn.Close()
		return nil, fmt.Errorf("mdns osc: %w", err)
	}
	mdnsJSON, err := zeroconf.RegisterProxy(name, "_oscjson._tcp", "local.", httpL.Addr().(*net.TCPAddr).Port, "localhost.local.", []string{"127.0.0.1"}, []string{"txtvers=1"}, nil)
	if err != nil {
		mdnsOSC.Shutdown()
		srv.Shutdown(context.Background())
		oscConn.Close()
		return nil, fmt.Errorf("mdns oscjson: %w", err)
	}

	return &oscquery{
		oscConn:      oscConn,
		httpListener: httpL,
		httpServer:   srv,
		mdnsOSC:      mdnsOSC,
		mdnsJSON:     mdnsJSON,
	}, nil
}

func (q *oscquery) Shutdown() {
	if q.mdnsJSON != nil {
		q.mdnsJSON.Shutdown()
	}
	if q.mdnsOSC != nil {
		q.mdnsOSC.Shutdown()
	}
	if q.httpServer != nil {
		q.httpServer.Shutdown(context.Background())
	}
	if q.oscConn != nil {
		q.oscConn.Close()
	}
}

// hostInfo and oscNode use ALL_CAPS field names per the OSCQuery spec.
type hostInfo struct {
	Name         string          `json:"NAME"`
	OSCIP        string          `json:"OSC_IP"`
	OSCPort      int             `json:"OSC_PORT"`
	OSCTransport string          `json:"OSC_TRANSPORT"`
	Extensions   map[string]bool `json:"EXTENSIONS"`
}

type oscNode struct {
	Description string             `json:"DESCRIPTION,omitempty"`
	FullPath    string             `json:"FULL_PATH"`
	Access      int                `json:"ACCESS"`
	Type        string             `json:"TYPE,omitempty"`
	Contents    map[string]oscNode `json:"CONTENTS,omitempty"`
}

func rootNode() oscNode {
	return oscNode{
		Description: "oscsound",
		FullPath:    "/",
		Contents: map[string]oscNode{
			"avatar": {
				FullPath: "/avatar",
				Access:   2,
				Contents: map[string]oscNode{
					"parameters": {FullPath: "/avatar/parameters", Access: 2},
					"change":     {FullPath: "/avatar/change", Access: 3, Type: "s"},
				},
			},
		},
	}
}

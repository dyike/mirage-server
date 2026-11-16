package rfb

import (
	"net"
	"net/netip"
	"time"
)

// macOS's push mode (AutoFramebufferUpdate) resends each changed display
// in full, every tick, in a quickly compressed form: measured on a
// two-display Mac, 8 MB a picture where answering an incremental request
// took 96 KB for the same screen. On a local network that buys pace; over
// a VPN or the internet a few Mbit/s take seconds per picture and the
// screen stays black. Push only on a directly attached network, and give
// it up when a pushed update arrives slowly.

// Config.Network values.
const (
	NetworkLocal  = "local"
	NetworkRemote = "remote"
)

// pushSlowUpdate is how long one pushed update may take to arrive before
// the client goes back to requesting updates.
const pushSlowUpdate = time.Second

// pushSuits reports whether push mode fits the link to the server. An
// explicit UpdateInterval always pushes.
func (c *Client) pushSuits() bool {
	return c.cfg.UpdateInterval > 0 || c.onLocalNetwork()
}

// onLocalNetwork reports whether the server is on a directly attached
// network, rather than behind a VPN or across the internet.
func (c *Client) onLocalNetwork() bool {
	switch c.cfg.Network {
	case NetworkLocal:
		return true
	case NetworkRemote:
		return false
	}
	tcp, ok := c.conn.RemoteAddr().(*net.TCPAddr)
	if !ok {
		return true
	}
	return onLink(tcp.AddrPort().Addr().Unmap(), localNetworks())
}

// localNetworks lists the networks of the interfaces that are up, leaving
// out point-to-point ones: VPN tunnels and proxies' virtual interfaces.
func localNetworks() []netip.Prefix {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var nets []netip.Prefix
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagPointToPoint != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok {
				if addr, ok := netip.AddrFromSlice(n.IP); ok {
					ones, _ := n.Mask.Size()
					nets = append(nets, netip.PrefixFrom(addr.Unmap(), ones))
				}
			}
		}
	}
	return nets
}

// onLink reports whether remote is on one of the given networks, or on
// this machine.
func onLink(remote netip.Addr, nets []netip.Prefix) bool {
	if remote.IsLoopback() {
		return true
	}
	// A Bonjour address such as fe80::1%en0: a prefix never contains an
	// address with a zone.
	remote = remote.WithZone("")
	for _, n := range nets {
		if n.Contains(remote) {
			return true
		}
	}
	return false
}

// checkPushPace runs on the reader after each update. A pushed update that
// took pushSlowUpdate to arrive ends push mode: any update request does,
// and the reader keeps InFlight of them outstanding from then on.
func (c *Client) checkPushPace(took time.Duration) error {
	if took < pushSlowUpdate || c.cfg.UpdateInterval > 0 || !c.pushing.Load() {
		return nil
	}
	c.trace("pushed update took %v: requesting updates instead", took.Round(time.Millisecond))
	c.pushing.Store(false)
	if c.media.Load() == nil {
		c.applyRFBEconomy()
	}
	inFlight := c.cfg.InFlight
	if inFlight <= 0 {
		inFlight = 4
	}
	// The run loop asks for one more after this update.
	for range inFlight - 1 {
		if err := c.requestUpdate(true); err != nil {
			return err
		}
	}
	return nil
}

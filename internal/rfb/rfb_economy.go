package rfb

import "sync/atomic"

// Off the local network RFB pictures cost what the link carries: a Mac with
// a 5K display and a second one sends 28 million pixels a full picture.
// When RFB pictures are the stream there, the client asks the server to
// scale by half, unless the viewer chose a scale. Main-display selection
// applies to all Mac sessions. Through a 4 Mbit/s link that cut the first
// picture from 0.24 MB in 1.4 s to 0.06 MB in 0.1 s.
type rfbEconomy struct {
	on        atomic.Bool // applied for this session
	picked    atomic.Bool // the viewer chose a display
	mainShown atomic.Bool
}

// economizeRFB applies the economy once, for a session showing RFB
// pictures off the local network.
func (c *Client) economizeRFB() {
	if !c.apple || c.onLocalNetwork() {
		return
	}
	c.applyRFBEconomy()
}

// applyRFBEconomy also handles slow links that address-based detection called
// local, such as a forwarded connection or a VPN with an on-link route.
func (c *Client) applyRFBEconomy() {
	if !c.apple || c.economy.on.Swap(true) {
		return
	}
	if c.cfg.AppleScale == 0 {
		c.trace("slow or remote RFB link: half scale")
		if err := c.appleScale(0.5); err != nil {
			return
		}
	}
	if l := c.layout.Load(); l != nil {
		c.showMainDisplay(l)
	}
}

// showMainDisplay defaults every multi-display Mac session to its main
// display, once. A later explicit choice, including AllDisplays, stands.
func (c *Client) showMainDisplay(l *DisplayLayout) {
	if c.economy.picked.Load() || l.Current != AllDisplays || len(l.Displays) < 2 {
		return
	}
	for _, d := range l.Displays {
		if d.Main && c.economy.mainShown.CompareAndSwap(false, true) {
			c.trace("default to main display %d", d.ID)
			c.selectDisplay(d.ID)
			return
		}
	}
}

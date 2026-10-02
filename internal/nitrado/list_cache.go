package nitrado

import "time"

// listCacheTTL is how long a directory listing is reused. One poll tick lists the selected log's
// directory up to three times (boot scan, newer-log check, size/modified check); within this
// window they share one request. It is kept well under the shortest poll interval (1s minimum,
// see killfeed.envPollInterval) so a later tick always sees a fresh listing.
const listCacheTTL = 750 * time.Millisecond

type cachedList struct {
	entries []fileServerListEntry
	at      time.Time
}

func listKey(serviceID, dir string) string { return serviceID + "\x00" + dir }

func (c *Client) cachedListing(serviceID, dir string) ([]fileServerListEntry, bool) {
	c.listMu.Lock()
	defer c.listMu.Unlock()
	hit, ok := c.listCache[listKey(serviceID, dir)]
	if !ok || time.Since(hit.at) > listCacheTTL {
		return nil, false
	}
	return append([]fileServerListEntry(nil), hit.entries...), true
}

func (c *Client) storeListing(serviceID, dir string, entries []fileServerListEntry) {
	c.listMu.Lock()
	defer c.listMu.Unlock()
	if c.listCache == nil {
		c.listCache = map[string]cachedList{}
	}
	for k, v := range c.listCache { // keep the map small: drop anything already expired
		if time.Since(v.at) > listCacheTTL {
			delete(c.listCache, k)
		}
	}
	c.listCache[listKey(serviceID, dir)] = cachedList{entries: append([]fileServerListEntry(nil), entries...), at: time.Now()}
}

// forgetListings drops every cached listing (after this client writes to the file server).
func (c *Client) forgetListings() {
	c.listMu.Lock()
	defer c.listMu.Unlock()
	c.listCache = nil
}

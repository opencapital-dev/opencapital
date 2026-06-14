package datakey

import "fmt"

// DataKey builds the canonical data.v2 Kafka key:
//
//	org_id | plugin_id | namespace | portfolio | source_id | observed_at
//
// portfolio is the portfolio UUID string, or "" for org-scoped data.
func DataKey(orgID, pluginID, namespace, portfolio, sourceID string, observedAt int64) []byte {
	return []byte(fmt.Sprintf("%s|%s|%s|%s|%s|%d", orgID, pluginID, namespace, portfolio, sourceID, observedAt))
}

// EventKey builds the canonical portfolio_events.v2 Kafka key:
//
//	org_id | plugin_id | source_id
func EventKey(orgID, pluginID, sourceID string) []byte {
	return []byte(fmt.Sprintf("%s|%s|%s", orgID, pluginID, sourceID))
}

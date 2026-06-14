package datakey

import "testing"

func TestDataKey(t *testing.T) {
	tests := []struct {
		name       string
		orgID      string
		pluginID   string
		namespace  string
		portfolio  string
		sourceID   string
		observedAt int64
		want       string
	}{
		{
			name:       "portfolio-scoped",
			orgID:      "o1",
			pluginID:   "yfinance-app",
			namespace:  "prices.ohlcv",
			portfolio:  "p1",
			sourceID:   "GKP",
			observedAt: 1700,
			want:       "o1|yfinance-app|prices.ohlcv|p1|GKP|1700",
		},
		{
			name:       "org-scoped empty portfolio",
			orgID:      "o1",
			pluginID:   "yfinance-app",
			namespace:  "prices.ohlcv",
			portfolio:  "",
			sourceID:   "GKP",
			observedAt: 1700,
			want:       "o1|yfinance-app|prices.ohlcv||GKP|1700",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := string(DataKey(tt.orgID, tt.pluginID, tt.namespace, tt.portfolio, tt.sourceID, tt.observedAt))
			if got != tt.want {
				t.Errorf("DataKey() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestEventKey(t *testing.T) {
	tests := []struct {
		name     string
		orgID    string
		pluginID string
		sourceID string
		want     string
	}{
		{
			name:     "trade event",
			orgID:    "o1",
			pluginID: "core-app",
			sourceID: "trade-uuid",
			want:     "o1|core-app|trade-uuid",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := string(EventKey(tt.orgID, tt.pluginID, tt.sourceID))
			if got != tt.want {
				t.Errorf("EventKey() = %q, want %q", got, tt.want)
			}
		})
	}
}

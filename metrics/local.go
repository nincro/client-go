package metrics

type Counter struct {
	NameSpace string
	Name      string
	Count     int64
}

var (
	TxnCounterLocal = Counter{
		NameSpace: "tikv",
		Name:      "txn_total",
		Count:     0,
	}
)

func (c Counter) Inc() {
	c.Count++
}


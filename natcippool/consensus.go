package ippool

import (
	"net/netip"
	"strconv"

	"github.com/tidwall/uhaha"
	"tailscale.com/tailcfg"
)

// StartConsensusMember has this node join the consensus protocol for handing out ip addresses
func StartConsensusMember(nodeID, addr, joinAddr string) {
	var conf uhaha.Config

	conf.Name = "natc"

	conf.InitialData = initData()

	// TODO is JSON on disk what we want?
	conf.UseJSONSnapshots = true

	conf.AddWriteCommand("ipcheckout", cmdCheckOut)
	//conf.AddWriteCommand("ipcheckin", cmdCheckIn)

	conf.NodeID = nodeID
	conf.Addr = addr
	if joinAddr != "" {
		conf.JoinAddr = joinAddr
	}

	uhaha.Main(conf)
}

func initData() *consensusData {
	return &consensusData{
		V4Ranges: []netip.Prefix{netip.MustParsePrefix("100.80.0.0/24")},
	}
}

func cmdCheckOut(m uhaha.Machine, args []string) (interface{}, error) {
	data := m.Data().(*consensusData)
	nid, err := strconv.Atoi(args[1]) // TODO probably not really how you get a NodeID from a string
	if err != nil {
		panic(err)
	}
	domain := args[2]
	return data.checkoutAddrForNode(tailcfg.NodeID(nid), domain)
}

//func cmdCheckIn(m uhaha.Machine, args []string) (interface{}, error) {
//return 0, nil
//}

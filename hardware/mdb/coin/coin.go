package coin

import (
	"context"
	"encoding/binary"
	"time"

	"github.com/juju/errors"
	"github.com/temoto/alive"
	"github.com/temoto/vender/currency"
	"github.com/temoto/vender/engine"
	"github.com/temoto/vender/hardware/mdb"
	"github.com/temoto/vender/hardware/money"
	"github.com/temoto/vender/head/state"
)

const (
	coinTypeCount = 16
)

//go:generate stringer -type=CoinRouting -trimprefix=Routing
type CoinRouting uint8

const (
	RoutingCashBox CoinRouting = 0
	RoutingTubes   CoinRouting = 1
	RoutingNotUsed CoinRouting = 2
	RoutingReject  CoinRouting = 3
)

//go:generate stringer -type=Features -trimprefix=Feature
type Features uint32

const (
	FeatureAlternativePayout Features = 1 << iota
	FeatureExtendedDiagnostic
	FeatureControlledManualFillPayout
	FeatureFTL
)

type CoinAcceptor struct {
	dev             mdb.Device
	dispenseTimeout time.Duration

	// Indicates the value of the bill types 0 to 15.
	// These are final values including all scaling factors.
	coinTypeCredit []currency.Nominal

	coinTypeRouting uint16

	featureLevel      uint8
	supportedFeatures Features

	internalScalingFactor int

	doReset engine.Doer
	doSetup engine.Doer
}

var (
	packetTubeStatus = mdb.MustPacketFromHex("0a", true)
	packetExpIdent   = mdb.MustPacketFromHex("0f00", true)
	packetDiagStatus = mdb.MustPacketFromHex("0f05", true)

	dispenseBusyPs = []mdb.Packet{
		mdb.MustPacketFromHex("02", true),
		mdb.MustPacketFromHex("0f", true),
	}
)

var (
	ErrNoCredit      = errors.Errorf("No Credit")
	ErrDoubleArrival = errors.Errorf("Double Arrival")
	ErrCoinRouting   = errors.Errorf("Coin Routing")
	ErrCoinJam       = errors.Errorf("Coin Jam")
	ErrSlugs         = errors.Errorf("Slugs")
)

func (self *CoinAcceptor) Init(ctx context.Context) error {
	// TODO read config
	self.dev.Init(ctx, 0x08, "coinacceptor", binary.BigEndian)
	self.dispenseTimeout = 10 * time.Second

	self.doReset = self.dev.NewDoReset()
	self.doSetup = self.newSetuper()

	self.coinTypeCredit = make([]currency.Nominal, coinTypeCount)
	self.internalScalingFactor = 1 // FIXME
	// TODO maybe execute CommandReset then wait for StatusWasReset
	err := self.newIniter().Do(ctx)
	return errors.Annotate(err, "hardware/mdb/coin/Init")
}

func (self *CoinAcceptor) SupportedNominals() []currency.Nominal {
	ns := make([]currency.Nominal, 0, len(self.coinTypeCredit))
	for _, n := range self.coinTypeCredit {
		if n > 0 {
			ns = append(ns, n)
		}
	}
	return ns
}

func (self *CoinAcceptor) Run(ctx context.Context, a *alive.Alive, fun func(money.PollItem)) {
	self.dev.PollLoopPassive(ctx, a, self.newPoller(fun))
}

func (self *CoinAcceptor) newPoller(fun func(money.PollItem)) mdb.PollParseFunc {
	return func(r mdb.PacketError) bool {
		if r.E != nil {
			return true
		}

		bs := r.P.Bytes()
		if len(bs) == 0 {
			return false
		}

		pi := money.PollItem{}
		skip := false
		for i, b := range bs {
			if skip {
				skip = false
				continue
			}
			b2 := byte(0)
			if i+1 < len(bs) {
				b2 = bs[i+1]
			}
			pi, skip = self.parsePollItem(b, b2)
			fun(pi)
		}
		return true
	}
}

func (self *CoinAcceptor) newIniter() engine.Doer {
	tx := engine.NewTransaction("coin-init")
	tx.Root.
		Append(self.doSetup).
		Append(engine.Func0{F: func() error {
			var err error
			// timeout is unfortunately common "response" for unsupported commands
			if err = self.CommandExpansionIdentification(); err != nil && !errors.IsTimeout(err) {
				return err
			}
			if err = self.CommandFeatureEnable(FeatureExtendedDiagnostic); err != nil && !errors.IsTimeout(err) {
				return err
			}
			diagResult := new(DiagResult)
			if err = self.CommandExpansionSendDiagStatus(diagResult); err != nil && !errors.IsTimeout(err) {
				return err
			}
			return nil
		}}).
		Append(engine.Func0{F: self.CommandTubeStatus}).
		Append(engine.Sleep{Duration: self.dev.DelayNext}).
		Append(engine.Func{F: func(ctx context.Context) error {
			config := state.GetConfig(ctx)
			// TODO read enabled nominals from config
			_ = config
			return self.CommandCoinType(0xffff, 0xffff).Do(ctx)
		}})
	return tx
}

func (self *CoinAcceptor) Restarter() engine.Doer {
	tx := engine.NewTransaction("coin-restart")
	tx.Root.
		Append(self.doReset).
		Append(engine.Sleep{Duration: self.dev.DelayNext}).
		Append(self.newIniter())
	return tx
}

func (self *CoinAcceptor) newSetuper() engine.Doer {
	return engine.Func{F: func(ctx context.Context) error {
		const expectLengthMin = 7
		err := self.dev.DoSetup(ctx)
		if err != nil {
			return errors.Annotate(err, "hardware/mdb/coin SETUP")
		}
		bs := self.dev.SetupResponse.Bytes()
		if len(bs) < expectLengthMin {
			return errors.Errorf("hardware/mdb/coin SETUP response=%s expected >= %d bytes", self.dev.SetupResponse.Format(), expectLengthMin)
		}
		self.featureLevel = bs[0]
		scalingFactor := bs[3]
		self.coinTypeRouting = self.dev.ByteOrder.Uint16(bs[5 : 5+2])
		for i, sf := range bs[7 : 7+16] {
			n := currency.Nominal(sf) * currency.Nominal(scalingFactor) * currency.Nominal(self.internalScalingFactor)
			self.dev.Log.Debugf("i=%d sf=%d nominal=%s", i, sf, currency.Amount(n).Format100I())
			self.coinTypeCredit[i] = n
		}
		self.dev.Log.Debugf("Changer Feature Level: %d", self.featureLevel)
		self.dev.Log.Debugf("Country / Currency Code: %x", bs[1:1+2])
		self.dev.Log.Debugf("Coin Scaling Factor: %d", scalingFactor)
		self.dev.Log.Debugf("Decimal Places: %d", bs[4])
		self.dev.Log.Debugf("Coin Type Routing: %b", self.coinTypeRouting)
		self.dev.Log.Debugf("Coin Type Credit: %x %#v", bs[7:], self.coinTypeCredit)
		return nil
	}}
}

func (self *CoinAcceptor) CommandTubeStatus() error {
	const expectLengthMin = 2
	request := packetTubeStatus
	r := self.dev.Tx(request)
	if r.E != nil {
		return errors.Annotate(r.E, "hardware/mdb/coin TUBE STATUS")
	}
	self.dev.Log.Debugf("tubestatus response=(%d)%s", r.P.Len(), r.P.Format())
	bs := r.P.Bytes()
	if len(bs) < expectLengthMin {
		return errors.Errorf("hardware/mdb/coin TUBE money.Status response=%s expected >= %d bytes", r.P.Format(), expectLengthMin)
	}
	full := self.dev.ByteOrder.Uint16(bs[0:2])
	counts := bs[2:18]
	self.dev.Log.Debugf("tubestatus full=%b counts=%v", full, counts)
	// TODO use full,counts
	_ = full
	_ = counts
	return nil
}

func (self *CoinAcceptor) CommandCoinType(accept, dispense uint16) engine.Doer {
	buf := [5]byte{0x0c}
	self.dev.ByteOrder.PutUint16(buf[1:], accept)
	self.dev.ByteOrder.PutUint16(buf[3:], dispense)
	request := mdb.MustPacketFromBytes(buf[:], true)
	return self.dev.NewTx(request)
}

func (self *CoinAcceptor) NewDispense(nominal currency.Nominal, count uint8) engine.Doer {
	tag := "coin/dispense"

	doDispense := engine.Func{Name: tag, F: func(ctx context.Context) error {
		if count >= 16 {
			return errors.Errorf("%s count=%d overflow >=16", tag, count)
		}
		coinType := self.nominalCoinType(nominal)
		if coinType < 0 {
			return errors.Errorf("%s not supported for nominal=%v", tag, currency.Amount(nominal).Format100I())
		}

		request := mdb.MustPacketFromBytes([]byte{0x0d, (count << 4) + uint8(coinType)}, true)
		err := self.dev.Tx(request).E
		return errors.Annotate(err, tag)
	}}

	tx := engine.NewTransaction(tag)
	tx.Root.Append(doDispense).Append(self.dev.NewPollUntilEmpty(tag, self.dispenseTimeout, dispenseBusyPs))
	return tx
}

func (self *CoinAcceptor) NewPayout(amount currency.Amount) engine.Doer {
	tag := "coin/payout"

	doPayout := engine.Func{Name: tag, F: func(ctx context.Context) error {
		// FIXME 100 magic number
		request := mdb.MustPacketFromBytes([]byte{0x0f, 0x02, byte(int(amount) / 100 / self.internalScalingFactor)}, true)
		err := self.dev.Tx(request).E
		return errors.Annotate(err, tag)
	}}

	tx := engine.NewTransaction(tag)
	tx.Root.Append(doPayout).Append(self.dev.NewPollUntilEmpty(tag, self.dispenseTimeout*4, dispenseBusyPs))
	return tx
}

func (self *CoinAcceptor) CommandExpansionIdentification() error {
	const expectLength = 33
	request := packetExpIdent
	r := self.dev.Tx(request)
	if r.E != nil {
		return errors.Annotate(r.E, "hardware/mdb/coin/CommandExpansionIdentification")
	}
	self.dev.Log.Debugf("EXPANSION IDENTIFICATION response=(%d)%s", r.P.Len(), r.P.Format())
	bs := r.P.Bytes()
	if len(bs) < expectLength {
		return errors.Errorf("hardware/mdb/coin EXPANSION IDENTIFICATION response=%s expected %d bytes", r.P.Format(), expectLength)
	}
	self.supportedFeatures = Features(self.dev.ByteOrder.Uint32(bs[29 : 29+4]))
	self.dev.Log.Debugf("Manufacturer Code: %x", bs[0:0+3])
	self.dev.Log.Debugf("Serial Number: '%s'", string(bs[3:3+12]))
	self.dev.Log.Debugf("Model #/Tuning Revision: '%s'", string(bs[15:15+12]))
	self.dev.Log.Debugf("Software Version: %x", bs[27:27+2])
	self.dev.Log.Debugf("Optional Features: %b", self.supportedFeatures)
	return nil
}

// CommandExpansionSendDiagStatus returns:
// - `nil` if command is not supported by device, result is not modified
// - otherwise returns nil or MDB/parse error, result set to valid DiagResult
func (self *CoinAcceptor) CommandExpansionSendDiagStatus(result *DiagResult) error {
	if self.supportedFeatures&FeatureExtendedDiagnostic == 0 {
		self.dev.Log.Debugf("CommandExpansionSendDiagStatus feature is not supported")
		return nil
	}
	r := self.dev.Tx(packetDiagStatus)
	if r.E != nil {
		return errors.Annotate(r.E, "hardware/mdb/coin/CommandExpansionSendDiagStatus")
	}
	dr, err := parseDiagResult(r.P.Bytes(), self.dev.ByteOrder)
	self.dev.Log.Debugf("DiagStatus=%s", dr.Error())
	if result != nil {
		*result = dr
	}
	return errors.Annotate(err, "hardware/mdb/coin/CommandExpansionSendDiagStatus")
}

func (self *CoinAcceptor) CommandFeatureEnable(requested Features) error {
	f := requested & self.supportedFeatures
	buf := [6]byte{0x0f, 0x01}
	self.dev.ByteOrder.PutUint32(buf[2:], uint32(f))
	request := mdb.MustPacketFromBytes(buf[:], true)
	err := self.dev.Tx(request).E
	return errors.Annotate(err, "hardware/mdb/coin/CommandFeatureEnable")
}

func (self *CoinAcceptor) coinTypeNominal(b byte) currency.Nominal {
	if b >= coinTypeCount {
		self.dev.Log.Errorf("invalid coin type: %d", b)
		return 0
	}
	return self.coinTypeCredit[b]
}

func (self *CoinAcceptor) nominalCoinType(nominal currency.Nominal) int8 {
	for ct, n := range self.coinTypeCredit {
		if n == nominal && ((1<<uint(ct))&self.coinTypeRouting != 0) {
			return int8(ct)
		}
	}
	return -1
}

func (self *CoinAcceptor) parsePollItem(b, b2 byte) (money.PollItem, bool) {
	switch b {
	case 0x01: // Escrow request
		return money.PollItem{Status: money.StatusReturnRequest}, false
	case 0x02: // Changer Payout Busy
		return money.PollItem{Status: money.StatusBusy}, false
	// high
	case 0x03: // No Credit
		return money.PollItem{Status: money.StatusError, Error: ErrNoCredit}, false
	// high
	case 0x04: // Defective Tube Sensor
		return money.PollItem{Status: money.StatusFatal, Error: money.ErrSensor}, false
	case 0x05: // Double Arrival
		return money.PollItem{Status: money.StatusInfo, Error: ErrDoubleArrival}, false
	// high
	case 0x06: // Acceptor Unplugged
		return money.PollItem{Status: money.StatusFatal, Error: money.ErrNoStorage}, false
	// high
	case 0x07: // Tube Jam
		return money.PollItem{Status: money.StatusFatal, Error: money.ErrJam}, false
	// high
	case 0x08: // ROM checksum error
		return money.PollItem{Status: money.StatusFatal, Error: money.ErrROMChecksum}, false
	// high
	case 0x09: // Coin Routing Error
		return money.PollItem{Status: money.StatusError, Error: ErrCoinRouting}, false
	case 0x0a: // Changer Busy
		return money.PollItem{Status: money.StatusBusy}, false
	case 0x0b: // Changer was Reset
		return money.PollItem{Status: money.StatusWasReset}, false
	// high
	case 0x0c: // Coin Jam
		return money.PollItem{Status: money.StatusFatal, Error: ErrCoinJam}, false
	case 0x0d: // Possible Credited Coin Removal
		return money.PollItem{Status: money.StatusError, Error: money.ErrFraud}, false
	}

	if b>>5 == 1 { // Slug count 001xxxxx
		slugs := b & 0x1f
		self.dev.Log.Debugf("Number of slugs: %d", slugs)
		return money.PollItem{Status: money.StatusInfo, Error: ErrSlugs, DataCount: slugs}, false
	}
	if b>>6 == 1 { // Coins Deposited
		// b=01yyxxxx b2=number of coins in tube
		// yy = coin routing
		// xxxx = coin type
		coinType := b & 0xf
		routing := CoinRouting((b >> 4) & 3)
		pi := money.PollItem{
			DataNominal: self.coinTypeNominal(coinType),
			DataCount:   1,
		}
		switch routing {
		case RoutingCashBox:
			pi.Status = money.StatusCredit
			pi.DataCashbox = true
		case RoutingTubes:
			pi.Status = money.StatusCredit
		case RoutingNotUsed:
			pi.Status = money.StatusError
			pi.Error = errors.Errorf("routing=notused b=%x pi=%s", b, pi.String())
		case RoutingReject:
			pi.Status = money.StatusRejected
		default:
			// pi.Status = money.StatusFatal
			panic(errors.Errorf("code error b=%x routing=%b", b, routing))
		}
		self.dev.Log.Debugf("deposited coinType=%d routing=%s pi=%s", coinType, routing.String(), pi.String())
		return pi, true
	}
	if b&0x80 != 0 { // Coins Dispensed Manually
		// b=1yyyxxxx b2=number of coins in tube
		// yyy = coins dispensed
		// xxxx = coin type
		count := (b >> 4) & 7
		nominal := self.coinTypeNominal(b & 0xf)
		return money.PollItem{Status: money.StatusDispensed, DataNominal: nominal, DataCount: count}, true
	}

	err := errors.Errorf("parsePollItem unknown=%x", b)
	return money.PollItem{Status: money.StatusFatal, Error: err}, false
}

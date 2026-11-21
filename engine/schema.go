package engine

import (
	"time"

	"github.com/victorcondackdev/fraud-rules-go/expr"
	"github.com/victorcondackdev/fraud-rules-go/velocity"
)

// Transaction is a Pix transfer awaiting a risk decision. Amounts are in
// cents. Party identifiers are opaque: use internal account IDs or hashed
// keys, never raw CPF/CNPJ, so the feature store holds no personal data.
type Transaction struct {
	ID          string      `json:"id"`
	Time        time.Time   `json:"time"`
	Amount      int64       `json:"amount"`
	Channel     string      `json:"channel,omitempty"`    // app, web, api, open_finance
	Initiation  string      `json:"initiation,omitempty"` // manual, key, qr_static, qr_dynamic, scheduled
	Description string      `json:"description,omitempty"`
	Payer       Payer       `json:"payer"`
	Beneficiary Beneficiary `json:"beneficiary"`

	// Label and Pattern are ground truth in history files used by
	// backtests; they are ignored when evaluating.
	Label   string `json:"label,omitempty"`   // "fraud" or "legit"
	Pattern string `json:"pattern,omitempty"` // injected fraud pattern, if any
}

// Payer is the sending side.
type Payer struct {
	ID             string    `json:"id"`
	Type           string    `json:"type,omitempty"` // PF (natural person) or PJ (company)
	AccountCreated time.Time `json:"account_created,omitempty"`
	// DayLimit and NightLimit are the per-transaction limits configured for
	// the customer, in cents.
	DayLimit   int64    `json:"day_limit,omitempty"`
	NightLimit int64    `json:"night_limit,omitempty"`
	Device     Device   `json:"device"`
	IP         string   `json:"ip,omitempty"`
	Tags       []string `json:"tags,omitempty"`
}

// Device is the device that initiated the transfer.
type Device struct {
	ID        string    `json:"id"`
	Trusted   bool      `json:"trusted"`
	FirstSeen time.Time `json:"first_seen,omitempty"`
}

// Beneficiary is the receiving side.
type Beneficiary struct {
	ID             string    `json:"id"`
	Type           string    `json:"type,omitempty"`
	Bank           string    `json:"bank,omitempty"`     // ISPB of the receiving institution
	KeyType        string    `json:"key_type,omitempty"` // cpf, cnpj, phone, email, evp
	KeyCreated     time.Time `json:"key_created,omitempty"`
	AccountCreated time.Time `json:"account_created,omitempty"`
	// FraudMarks counts fraud reports against the beneficiary known to the
	// PSP, e.g. from MED disputes or shared fraud markers.
	FraudMarks int64 `json:"fraud_marks,omitempty"`
}

// Field IDs of the Pix schema, in definition order.
const (
	fTxID = iota
	fTxAmount
	fTxTime
	fTxChannel
	fTxInitiation
	fTxDescription
	fPayerID
	fPayerType
	fPayerAccountCreated
	fPayerDayLimit
	fPayerNightLimit
	fPayerDeviceID
	fPayerDeviceTrusted
	fPayerDeviceFirstSeen
	fPayerIP
	fPayerTags
	fBenID
	fBenType
	fBenBank
	fBenKeyType
	fBenKeyCreated
	fBenAccountCreated
	fBenFraudMarks
	sPayer       // payer.transfers
	sPair        // pair.transfers
	sDevice      // device.transfers
	sIP          // ip.transfers
	sBeneficiary // beneficiary.received
	numFields
)

var pixFields = [numFields]struct {
	name string
	t    expr.Type
}{
	fTxID:                 {"tx.id", expr.String},
	fTxAmount:             {"tx.amount", expr.Int},
	fTxTime:               {"tx.time", expr.Time},
	fTxChannel:            {"tx.channel", expr.String},
	fTxInitiation:         {"tx.initiation", expr.String},
	fTxDescription:        {"tx.description", expr.String},
	fPayerID:              {"payer.id", expr.String},
	fPayerType:            {"payer.type", expr.String},
	fPayerAccountCreated:  {"payer.account_created", expr.Time},
	fPayerDayLimit:        {"payer.day_limit", expr.Int},
	fPayerNightLimit:      {"payer.night_limit", expr.Int},
	fPayerDeviceID:        {"payer.device.id", expr.String},
	fPayerDeviceTrusted:   {"payer.device.trusted", expr.Bool},
	fPayerDeviceFirstSeen: {"payer.device.first_seen", expr.Time},
	fPayerIP:              {"payer.ip", expr.String},
	fPayerTags:            {"payer.tags", expr.StringList},
	fBenID:                {"beneficiary.id", expr.String},
	fBenType:              {"beneficiary.type", expr.String},
	fBenBank:              {"beneficiary.bank", expr.String},
	fBenKeyType:           {"beneficiary.key_type", expr.String},
	fBenKeyCreated:        {"beneficiary.key_created", expr.Time},
	fBenAccountCreated:    {"beneficiary.account_created", expr.Time},
	fBenFraudMarks:        {"beneficiary.fraud_marks", expr.Int},
	sPayer:                {"payer.transfers", expr.Stream},
	sPair:                 {"pair.transfers", expr.Stream},
	sDevice:               {"device.transfers", expr.Stream},
	sIP:                   {"ip.transfers", expr.Stream},
	sBeneficiary:          {"beneficiary.received", expr.Stream},
}

// moneyFields are rendered as reais in explanations.
var moneyFields = map[string]bool{"tx.amount": true, "payer.day_limit": true, "payer.night_limit": true}

var pixSchema = func() *expr.Schema {
	s := expr.NewSchema()
	for i, f := range pixFields {
		if id := s.Define(f.name, f.t); id != i {
			panic("engine: schema field IDs out of order")
		}
	}
	return s
}()

// Schema returns the schema rules are compiled against. Streams are:
//
//	payer.transfers       transfers sent by the payer; distinct = beneficiaries
//	pair.transfers        transfers from this payer to this beneficiary
//	device.transfers      transfers from the device; distinct = payers
//	ip.transfers          transfers from the IP; distinct = payers
//	beneficiary.received  transfers received by the beneficiary; distinct = payers
func Schema() *expr.Schema { return pixSchema }

// streamKey returns the feature store key and counterparty of stream s for
// tx. An empty key means the transaction has no such party.
func streamKey(s int, tx *Transaction) (key, counterparty string) {
	switch s {
	case sPayer:
		if tx.Payer.ID != "" {
			return "p:" + tx.Payer.ID, tx.Beneficiary.ID
		}
	case sPair:
		if tx.Payer.ID != "" && tx.Beneficiary.ID != "" {
			return "pb:" + tx.Payer.ID + ">" + tx.Beneficiary.ID, ""
		}
	case sDevice:
		if tx.Payer.Device.ID != "" {
			return "d:" + tx.Payer.Device.ID, tx.Payer.ID
		}
	case sIP:
		if tx.Payer.IP != "" {
			return "ip:" + tx.Payer.IP, tx.Payer.ID
		}
	case sBeneficiary:
		if tx.Beneficiary.ID != "" {
			return "b:" + tx.Beneficiary.ID, tx.Payer.ID
		}
	}
	return "", ""
}

// FeatureStore is the velocity backend the engine reads and writes.
// [*velocity.Store] implements it; tests substitute slow or failing ones.
type FeatureStore interface {
	Record(e velocity.Event)
	// Query answers count, sum and distinct.
	Query(key string, window time.Duration, now time.Time) (velocity.Stats, error)
	// QueryCounts answers count and sum only; it is cheaper on busy keys.
	QueryCounts(key string, window time.Duration, now time.Time) (velocity.Stats, error)
	MaxWindow() time.Duration
}

type aggEntry struct {
	stream   int
	window   time.Duration
	distinct bool // stats include Distinct
	stats    velocity.Stats
	err      error
}

// txEnv implements expr.Env for one transaction. Velocity answers are
// memoized, so a window queried by several rules costs one lookup.
type txEnv struct {
	tx    *Transaction
	store FeatureStore
	now   int64
	cache []aggEntry
	keys  [sBeneficiary - sPayer + 1]string
	keyOK [sBeneficiary - sPayer + 1]bool
}

func (e *txEnv) reset(tx *Transaction, store FeatureStore) {
	e.tx, e.store, e.now = tx, store, expr.UnixNano(tx.Time)
	e.cache = e.cache[:0]
	e.keyOK = [len(e.keyOK)]bool{}
}

func (e *txEnv) key(stream int) string {
	i := stream - sPayer
	if i < 0 || i >= len(e.keys) {
		return ""
	}
	if !e.keyOK[i] {
		e.keys[i], _ = streamKey(stream, e.tx)
		e.keyOK[i] = true
	}
	return e.keys[i]
}

func (e *txEnv) Int(id int) int64 {
	t := e.tx
	switch id {
	case fTxAmount:
		return t.Amount
	case fTxTime:
		return e.now
	case fPayerAccountCreated:
		return expr.UnixNano(t.Payer.AccountCreated)
	case fPayerDayLimit:
		return t.Payer.DayLimit
	case fPayerNightLimit:
		return t.Payer.NightLimit
	case fPayerDeviceFirstSeen:
		return expr.UnixNano(t.Payer.Device.FirstSeen)
	case fBenKeyCreated:
		return expr.UnixNano(t.Beneficiary.KeyCreated)
	case fBenAccountCreated:
		return expr.UnixNano(t.Beneficiary.AccountCreated)
	case fBenFraudMarks:
		return t.Beneficiary.FraudMarks
	}
	return 0
}

func (e *txEnv) Bool(id int) bool {
	return id == fPayerDeviceTrusted && e.tx.Payer.Device.Trusted
}

func (e *txEnv) String(id int) string {
	t := e.tx
	switch id {
	case fTxID:
		return t.ID
	case fTxChannel:
		return t.Channel
	case fTxInitiation:
		return t.Initiation
	case fTxDescription:
		return t.Description
	case fPayerID:
		return t.Payer.ID
	case fPayerType:
		return t.Payer.Type
	case fPayerDeviceID:
		return t.Payer.Device.ID
	case fPayerIP:
		return t.Payer.IP
	case fBenID:
		return t.Beneficiary.ID
	case fBenType:
		return t.Beneficiary.Type
	case fBenBank:
		return t.Beneficiary.Bank
	case fBenKeyType:
		return t.Beneficiary.KeyType
	}
	return ""
}

func (e *txEnv) List(id int) []string {
	if id == fPayerTags {
		return e.tx.Payer.Tags
	}
	return nil
}

func (e *txEnv) Now() int64 { return e.now }

func (e *txEnv) Aggregate(a expr.Aggregate) (int64, error) {
	var st velocity.Stats
	var err error
	found := false
	wantDistinct := a.Op == expr.AggDistinct
	for i := range e.cache {
		if c := &e.cache[i]; c.stream == a.StreamID && c.window == a.Window && (c.distinct || !wantDistinct) {
			st, err, found = c.stats, c.err, true
			break
		}
	}
	if !found {
		if key := e.key(a.StreamID); key != "" && e.store != nil {
			if wantDistinct {
				st, err = e.store.Query(key, a.Window, e.tx.Time)
			} else {
				st, err = e.store.QueryCounts(key, a.Window, e.tx.Time)
			}
		}
		e.cache = append(e.cache, aggEntry{stream: a.StreamID, window: a.Window, distinct: wantDistinct, stats: st, err: err})
	}
	if err != nil {
		return 0, err
	}
	switch a.Op {
	case expr.AggSum:
		return st.Sum, nil
	case expr.AggDistinct:
		return st.Distinct, nil
	}
	return st.Count, nil
}

// record writes a settled transaction to every stream it belongs to.
func record(store FeatureStore, tx *Transaction) {
	for s := sPayer; s <= sBeneficiary; s++ {
		key, cp := streamKey(s, tx)
		if key == "" {
			continue
		}
		store.Record(velocity.Event{Key: key, Time: tx.Time, Amount: tx.Amount, Counterparty: cp})
	}
}

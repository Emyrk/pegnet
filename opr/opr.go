// Copyright (c) of parts are held by the various contributors (see the CLA)
// Licensed under the MIT License. See LICENSE file in the project root for full license information.

package opr

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/FactomProject/btcutil/base58"
	"github.com/FactomProject/factom"
	"github.com/dustin/go-humanize"
	"github.com/pegnet/LXRHash"
	"github.com/pegnet/pegnet/common"
	"github.com/pegnet/pegnet/polling"
	log "github.com/sirupsen/logrus"
	"github.com/zpatrick/go-config"
)

// OraclePriceRecord is the data used and created by miners
type OraclePriceRecord struct {
	// These fields are not part of the OPR, but track values associated with the OPR.
	Config     *config.Config    `json:"-"` //  The config of the miner using the record
	Difficulty uint64            `json:"-"` // The difficulty of the given nonce
	Grade      float64           `json:"-"` // The grade when OPR records are compared
	OPRHash    []byte            `json:"-"` // The hash of the OPR record (used by PegNet Mining)
	EC         *factom.ECAddress `json:"-"` // Entry Credit Address used by a miner
	Entry      *factom.Entry     `json:"-"` // Entry to record this record
	StopMining chan int          `json:"-"` // Bool that stops PegNet Mining this OPR

	// These values define the context of the OPR, and they go into the PegNet OPR record, and are mined.
	OPRChainID         string     `json:"oprchainid"`      // [base58]  Chain ID of the chain used by the Oracle Miners
	Dbht               int32      `json:"dbht"`            //           The Directory Block Height of the OPR.
	WinPreviousOPR     [10]string `json:"winners"`         // First 8 bytes of the Entry Hashes of the previous winners
	CoinbasePNTAddress string     `json:"coinbase"`        // [base58]  PNT Address to pay PNT
	FactomDigitalID    []string   `json:"FactomDigitalID"` // [unicode] Digital Identity of the miner

	// The Oracle values of the OPR, they are the meat of the OPR record, and are mined.
	Assets OraclePriceRecordAssetList `json:"assets"`
}

func NewOraclePriceRecord() *OraclePriceRecord {
	o := new(OraclePriceRecord)
	o.Assets = make(OraclePriceRecordAssetList)

	return o
}

// LX holds an instance of lxrhash
var LX lxr.LXRHash
var lxInitializer sync.Once

// The init function for LX is expensive. So we should explicitly call the init if we intend
// to use it. Make the init call idempotent
func InitLX() {
	lxInitializer.Do(func() {
		// This code will only be executed ONCE, no matter how often you call it
		LX.Init(0xfafaececfafaecec, 25, 256, 5)
	})
}

// OPRChainID is the calculated chain id of the records chain
var OPRChainID string

// Token is a combination of currency code and value
type Token struct {
	code  string
	value float64
}

func check(e error) {
	if e != nil {
		log.WithError(e).Fatal("An error in OPR was encountered")
	}
}

// Validate performs sanity checks of the structure and values of the OPR.
// It does not validate the winners of the previous block.
func (opr *OraclePriceRecord) Validate(c *config.Config) bool {

	protocol, err1 := c.String("Miner.Protocol")
	network, err2 := c.String("Miner.Network")
	if err1 != nil || err2 != nil {
		return false
	}

	if len(OPRChainID) == 0 {
		OPRChainID = base58.Encode(common.ComputeChainIDFromStrings([]string{protocol, network, common.OPRChainTag}))
	}

	if opr.OPRChainID != OPRChainID {
		return false
	}

	pre, _, err := common.ConvertPegAddrToRaw(opr.CoinbasePNTAddress)
	if err != nil || pre != "tPNT" {
		return false
	}

	// Validate there are no 0's
	for k, v := range opr.Assets {
		if v == 0 && k != "PNT" { // PNT is exception until we get a value for it
			return false
		}
	}

	// Validate all the Assets exists
	if !opr.Assets.Contains(common.AllAssets) {
		return false // Missing some assets!
	}

	return true
}

// GetTokens creates an iterateable slice of Tokens containing all the currency values
func (opr *OraclePriceRecord) GetTokens() (tokens []Token) {
	return opr.Assets.List()
}

// GetHash returns the LXHash over the OPR's json representation
func (opr *OraclePriceRecord) GetHash() []byte {
	data, err := json.Marshal(opr)
	check(err)
	oprHash := LX.Hash(data)
	return oprHash
}

// ComputeDifficulty gets the difficulty by taking the hash of the OPRHash
// appended by the nonce. The difficulty is the highest 8 bytes of the hash
// taken as uint64 in Big Endian
func (opr *OraclePriceRecord) ComputeDifficulty(nonce []byte) (difficulty uint64) {
	no := append(opr.OPRHash, nonce...)
	h := LX.Hash(no)

	// The high eight bytes of the hash(hash(entry.Content) + nonce) is the difficulty.
	// Because we don't have a difficulty bar, we can define difficulty as the greatest
	// value, rather than the minimum value.  Our bar is the greatest difficulty found
	// within a 10 minute period.  We compute difficulty as Big Endian.
	for i := uint64(0); i < 8; i++ {
		difficulty = difficulty<<8 + uint64(h[i])
	}
	return difficulty
}

// Mine calculates difficulties with varying nonces, keeping track of the
// highest difficulty achieved in the Difficulty and ExtID[0] fields
// Stops when a signal is received on the StopMining channel.
func (opr *OraclePriceRecord) Mine(verbose bool) {

	// Pick a new nonce as a starting point.  Take time + last best nonce and hash that.
	nonce := []byte{0, 0}
	log.WithFields(log.Fields{"opr_hash": hex.EncodeToString(opr.OPRHash)}).Debug("Started mining")

	var i uint64
	var diff uint64
miningloop:
	for i = 0; ; i++ {
		select {
		case <-opr.StopMining:
			break miningloop

		default:
		}
		nonce = nonce[:0]
		for j := i; j > 0; j = j >> 8 {
			nonce = append(nonce, byte(j))
		}
		diff = opr.ComputeDifficulty(nonce)

		if diff > opr.Difficulty {
			opr.Difficulty = diff
			// Copy over the previous nonce
			opr.Entry.ExtIDs[0] = append(opr.Entry.ExtIDs[0][:0], nonce...)
			log.WithFields(log.Fields{
				"opr_hash":   hex.EncodeToString(opr.OPRHash),
				"difficulty": diff,
				"nonce":      hex.EncodeToString(nonce),
			}).Debug("Mined OPR")
		}
	}
	common.Stats.Update(i, opr.Difficulty)
}

// ShortString returns a human readable string with select data
func (opr *OraclePriceRecord) ShortString() string {

	fdid := strings.Join(opr.FactomDigitalID, "-")

	str := fmt.Sprintf("DID %30x OPRHash %30x Nonce %33x Difficulty %15x Grade %20f",
		fdid,
		opr.OPRHash,
		opr.Entry.ExtIDs[0],
		opr.Difficulty,
		opr.Grade)
	return str
}

// String returns a human readable string for the Oracle Record
func (opr *OraclePriceRecord) String() (str string) {
	str = fmt.Sprintf("Nonce %x\n", opr.Entry.ExtIDs[0])
	str = str + fmt.Sprintf("%32s %v\n", "OPRChainID", opr.OPRChainID)
	str = str + fmt.Sprintf("%32s %v\n", "Difficulty", opr.Difficulty)
	str = str + fmt.Sprintf("%32s %v\n", "Directory Block Height", opr.Dbht)
	str = str + fmt.Sprintf("%32s %v\n", "WinningPreviousOPRs", "")
	for i, v := range opr.WinPreviousOPR {
		str = str + fmt.Sprintf("%32s %2d, %s\n", "", i+1, v)
	}
	str = str + fmt.Sprintf("%32s %s\n", "Coinbase PNT", opr.CoinbasePNTAddress)

	// Make a display string out of the Digital Identity.

	str = str + fmt.Sprintf("%32s %v\n", "FactomDigitalID", strings.Join(opr.FactomDigitalID, "-"))
	for _, asset := range opr.Assets.List() {
		str = str + fmt.Sprintf("%32s %v\n", "PNT", asset)
	}

	str = str + fmt.Sprintf("\nWinners\n\n")

	pwin := GetPreviousOPRs(opr.Dbht - 1)

	// If there were previous winners, we need to make sure this miner is running
	// the software to detect them, and that we agree with their conclusions.
	if pwin != nil {
		for i, v := range opr.WinPreviousOPR {
			fid := ""
			for j, field := range pwin[i].FactomDigitalID {
				if j > 0 {
					fid = fid + "---"
				}
				fid = fid + field
			}
			balance := GetBalance(pwin[i].CoinbasePNTAddress)
			hbal := humanize.Comma(balance)
			str = str + fmt.Sprintf("   %16s %16x %30s %-56s = %10s\n",
				v,
				pwin[i].Entry.Hash()[:8],
				fid,
				pwin[i].
					CoinbasePNTAddress, hbal)
		}
	}
	return str
}

// LogFieldsShort returns a set of common fields to be included in logrus
func (opr *OraclePriceRecord) LogFieldsShort() log.Fields {
	did := strings.Join(opr.FactomDigitalID, "-")
	return log.Fields{
		"did":        did,
		"opr_hash":   hex.EncodeToString(opr.OPRHash),
		"nonce":      hex.EncodeToString(opr.Entry.ExtIDs[0]),
		"difficulty": opr.Difficulty,
		"grade":      opr.Grade,
	}
}

// SetPegValues assigns currency polling values to the OPR
func (opr *OraclePriceRecord) SetPegValues(assets polling.PegAssets) {
	for asset, v := range assets {
		opr.Assets[asset] = v.Value
	}
}

// NewOpr collects all the information unique to this miner and its configuration, and also
// goes and gets the oracle data.  Also collects the winners from the prior block and
// puts their entry hashes (base58) into this OPR
func NewOpr(minerNumber int, dbht int32, c *config.Config, alert chan *OPRs) (*OraclePriceRecord, error) {
	opr := NewOraclePriceRecord()

	// create the channel to stop pegnetMining
	opr.StopMining = make(chan int, 1)

	// Save the config object
	opr.Config = c

	// Get the Entry Credit Address that we need to write our OPR records.
	if ecadrStr, err := c.String("Miner.ECAddress"); err != nil {
		return nil, err
	} else {
		ecAdr, err := factom.FetchECAddress(ecadrStr)
		if err != nil {
			return nil, err
		}
		opr.EC = ecAdr
	}

	// Get the Identity Chain Specification
	if chainID58, err := c.String("Miner.IdentityChain"); err != nil {
		return nil, errors.New("config file has no Miner.IdentityChain specified")
	} else {
		fields := strings.Split(chainID58, ",")
		if minerNumber > 0 {
			fields = append(fields, fmt.Sprintf("miner%03d", minerNumber))
		}
		opr.FactomDigitalID = fields
	}

	// Get the protocol chain to be used for pegnetMining records
	protocol, err1 := c.String("Miner.Protocol")
	network, err2 := c.String("Miner.Network")
	if err1 != nil {
		return nil, errors.New("config file has no Miner.Protocol specified")
	}
	if err2 != nil {
		return nil, errors.New("config file has no Miner.Network specified")
	}
	opr.OPRChainID = base58.Encode(common.ComputeChainIDFromStrings([]string{protocol, network, common.OPRChainTag}))

	opr.Dbht = dbht

	// If this is a test network, then give multiple miners their own tPNT address
	// because that is way more useful debugging than giving all miners the same
	// PNT address.  Otherwise, give all miners the same PNT address because most
	// users really doing mining will mostly be happen sending rewards to a single
	// address.
	if network == "TestNet" && minerNumber != 0 {
		fct := common.DebugFCTaddresses[minerNumber][1]
		sraw, err := common.ConvertUserStrFctEcToAddress(fct)
		if err != nil {
			return nil, err
		}
		raw, err := hex.DecodeString(sraw)
		if err != nil {
			return nil, err
		}
		opr.CoinbasePNTAddress, err = common.ConvertRawAddrToPeg("tPNT", raw)
		if err != nil {
			return nil, err
		}
	} else {
		if str, err := c.String("Miner.CoinbasePNTAddress"); err != nil {
			return nil, errors.New("config file has no Coinbase PNT Address")
		} else {
			opr.CoinbasePNTAddress = str
		}
	}
	winners := <-alert
	for i, w := range winners.ToBePaid {
		opr.WinPreviousOPR[i] = hex.EncodeToString(w.Entry.Hash()[:8])
	}

	opr.GetOPRecord(c)

	return opr, nil
}

// GetOPRecord initializes the OPR with polling data and factom entry
func (opr *OraclePriceRecord) GetOPRecord(c *config.Config) {
	opr.Config = c
	//get asset values
	var Peg polling.PegAssets
	Peg = polling.PullPEGAssets(c)
	opr.SetPegValues(Peg)

	var err error
	opr.Entry = new(factom.Entry)
	opr.Entry.ChainID = hex.EncodeToString(base58.Decode(opr.OPRChainID))
	opr.Entry.ExtIDs = [][]byte{{}}
	opr.Entry.Content, err = json.Marshal(opr)
	if err != nil {
		panic(err)
	}
	opr.OPRHash = LX.Hash(opr.Entry.Content)
}

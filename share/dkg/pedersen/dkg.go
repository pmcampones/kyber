// Package dkg implements a general distributed key generation (DKG) framework. This
// package serves two functionalities: (1) to run a fresh new DKG from scratch
// and (2) to reshare old shares to a potentially distinct new set of nodes (the
// "resharing" protocol). The
// former protocol is described in "A threshold cryptosystem without a trusted
// party" by Torben Pryds Pedersen. https://dl.acm.org/citation.cfm?id=1754929.
// The latter protocol is implemented in "Verifiable Secret Redistribution for
// Threshold Signing Schemes", by T. Wong et
// al.(https://www.cs.cmu.edu/~wing/publications/Wong-Wing02b.pdf)
package dkg

import (
	"errors"
	"fmt"

	"github.com/dedis/kyber"

	"github.com/dedis/kyber/share"
	vss "github.com/dedis/kyber/share/vss/pedersen"
	"github.com/dedis/kyber/sign/schnorr"
)

// Suite wraps the functionalities needed by the dkg package
type Suite vss.Suite

// Config holds all required information to run a fresh DKG protocol or a
// resharing protocol. In the case of a new fresh DKG protocol, one must fill
// the following fields: Suite, Longterm, NewNodes, Threshold (opt). In the case
// of a resharing protocol, one must fill the following: Suite, Longterm,
// OldNodes, NewNodes. If the node using this config is creating new shares
// (i.e. it belongs to the current group), the Share field must be filled in
// with the current share of the node. If the node using this config is a new
// addition and thus has no current share, the PublicCoeffs field be must be
// filled in.
type Config struct {
	Suite Suite

	// Longterm is the longterm secret key.
	Longterm kyber.Scalar

	// Current group of share holders. It will be nil for new DKG. These nodes
	// will have invalid shares after the protocol has been run. To be able to issue
	// new shares to a new group, the group member's public key must be inside this
	// list and in the Share field. Keys can be disjoint or not with respect to the
	// NewNodes list.
	OldNodes []kyber.Point

	// PublicCoeffs are the coefficients of the distributed polynomial needed
	// during the resharing protocol. The first coefficient is the key. It is
	// required for new share holders.  It should be nil for new DKG.
	PublicCoeffs []kyber.Point

	// Expected new group of share holders. These public-key designated nodes
	// will be in possession of new shares after the protocol has been ran. To be a
	// receiver a of new share, one's public key must be inside this list. Keys
	// can be disjoint or not with respect to the OldNodes list.
	NewNodes []kyber.Point

	// Share to refresh. It must be nil for a new node wishing to
	// join or create a group. To be able to issue new fresh shares to a new group,
	// one's share must be specified here, along with the public key inside the
	// OldNodes field.
	Share *DistKeyShare

	// New threshold to use in order to reconstruct the secret with the produced
	// shares. This threshold is with respect to the number of nodes in the
	// NewNodes list. If unspecified, default is set to
	// `vss.MinimumT(len(NewNodes))`. This threshold indicates the degree of the
	// polynomials used to create the shares, and the minimum number of
	// verification required for each deal.
	NewThreshold int

	// OldThreshold holds the threshold value that was used in the previous
	// configuration. This field MUST be specified when doing resharing, but is
	// not needed when doing a fresh DKG. This value is required to gather a
	// correct number of valid deals before creating the distributed key share.
	// NOTE: this field is always required (instead of taking the default when
	// absent) when doing a resharing to avoid a downgrade attack, where a resharing
	// the number of deals required is less than what it is supposed to be.
	OldThreshold int
}

// DistKeyGenerator is the struct that runs the DKG protocol.
type DistKeyGenerator struct {
	// config driving the behavior of DistKeyGenerator
	c     *Config
	suite Suite

	long   kyber.Scalar
	pub    kyber.Point
	dpub   *share.PubPoly
	dealer *vss.Dealer
	// verifiers indexed by dealer index
	verifiers map[uint32]*vss.Verifier
	rcvdDeals map[uint32]bool
	// performs the part of the response verification for old nodes
	oldAggregators map[uint32]*vss.Aggregator
	// index in the old list of nodes
	oidx int
	// index in the new list of nodes
	nidx int
	// old threshold used in the previous DKG
	oldT int
	// new threshold to use in this round
	newT int
	// indicates whether we are in the re-sharing protocol or basic DKG
	isResharing bool
	// indicates whether we are able to issue shares or not
	canIssue bool
	// Indicates whether we are able to receive a new share or not
	canReceive bool
	// indicates whether the node holding the pub key is present in the new list
	newPresent bool
	// indicates whether the node is present in the old list
	oldPresent bool
	// already processed our own deal
	processed bool
}

// NewDistKeyHandler takes a Config and returns a DistKeyGenerator that is able
// to drive the DKG or resharing protocol.
func NewDistKeyHandler(c *Config) (*DistKeyGenerator, error) {
	if c.NewNodes == nil && c.OldNodes == nil {
		return nil, errors.New("dkg: can't run with empty node list")
	}

	var isResharing bool
	if c.Share != nil || c.PublicCoeffs != nil {
		isResharing = true
	}
	if isResharing {
		if c.OldNodes == nil {
			return nil, errors.New("dkg: resharing config needs old nodes list")
		}
		if c.OldThreshold == 0 {
			return nil, errors.New("dkg: resharing case needs old threshold field")
		}
	}
	// canReceive is true by default since in the default DKG mode everyone
	// participates
	var canReceive = true
	pub := c.Suite.Point().Mul(c.Longterm, nil)
	oidx, oldPresent := findPub(c.OldNodes, pub)
	nidx, newPresent := findPub(c.NewNodes, pub)
	if !oldPresent && !newPresent {
		return nil, errors.New("dkg: public key not found in old list or new list")
	}

	var newThreshold int
	if c.NewThreshold != 0 {
		newThreshold = c.NewThreshold
	} else {
		newThreshold = vss.MinimumT(len(c.NewNodes))
	}

	var dealer *vss.Dealer
	var err error
	var canIssue bool
	if c.Share != nil {
		// resharing case
		secretCoeff := c.Share.Share.V
		dealer, err = vss.NewDealer(c.Suite, c.Longterm, secretCoeff, c.NewNodes, newThreshold)
		canIssue = true
	} else if !isResharing && newPresent {
		// fresh DKG case
		secretCoeff := c.Suite.Scalar().Pick(c.Suite.RandomStream())
		dealer, err = vss.NewDealer(c.Suite, c.Longterm, secretCoeff, c.NewNodes, newThreshold)
		canIssue = true
		// so the logic stays the same for the rest of the dkg code
		c.OldNodes = c.NewNodes
		oidx, oldPresent = findPub(c.OldNodes, pub)
	}

	if err != nil {
		return nil, err
	}

	var dpub *share.PubPoly
	var oldThreshold int
	if !newPresent {
		// if we are not in the new list of nodes, then we definitely can't
		// receive anything
		canReceive = false
	} else if isResharing && newPresent {
		if c.PublicCoeffs == nil && c.Share == nil {
			return nil, errors.New("dkg: can't receive new shares without the public polynomial")
		} else if c.PublicCoeffs != nil {
			dpub = share.NewPubPoly(c.Suite, c.Suite.Point().Base(), c.PublicCoeffs)
		} else if c.Share != nil {
			// take the commits of the share, no need to duplicate information
			c.PublicCoeffs = c.Share.Commits
			dpub = share.NewPubPoly(c.Suite, c.Suite.Point().Base(), c.PublicCoeffs)
		}
		// oldThreshold is only useful in the context of a new share holder, to
		// make sure there are enough correct deals from the old nodes.
		canReceive = true
		oldThreshold = len(c.PublicCoeffs)
	}
	dkg := &DistKeyGenerator{
		dealer:         dealer,
		oldAggregators: make(map[uint32]*vss.Aggregator),
		suite:          c.Suite,
		long:           c.Longterm,
		pub:            pub,
		canReceive:     canReceive,
		canIssue:       canIssue,
		isResharing:    isResharing,
		dpub:           dpub,
		oidx:           oidx,
		nidx:           nidx,
		c:              c,
		oldT:           oldThreshold,
		newT:           newThreshold,
		newPresent:     newPresent,
		oldPresent:     oldPresent,
	}
	if newPresent {
		err = dkg.initVerifiers(c)
	}
	return dkg, err
}

// NewDistKeyGenerator returns a dist key generator ready to create a fresh
// distributed key with the regular DKG protocol.
func NewDistKeyGenerator(suite Suite, longterm kyber.Scalar, participants []kyber.Point, t int) (*DistKeyGenerator, error) {
	c := &Config{
		Suite:        suite,
		Longterm:     longterm,
		NewNodes:     participants,
		NewThreshold: t,
	}
	return NewDistKeyHandler(c)
}

// Deals returns all the deals that must be broadcasted to all participants in
// the new list. The deal corresponding to this DKG is already added to this DKG
// and is ommitted from the returned map. To know which participant a deal
// belongs to, loop over the keys as indices in the list of new participants:
//
//   for i,dd := range distDeals {
//      sendTo(participants[i],dd)
//   }
//
// If this method cannot process its own Deal, that indicates a
// sever problem with the configuration or implementation and
// results in a panic.
func (d *DistKeyGenerator) Deals() (map[int]*Deal, error) {
	deals, err := d.dealer.EncryptedDeals()
	if err != nil {
		return nil, err
	}
	dd := make(map[int]*Deal)
	for i := range d.c.NewNodes {
		distd := &Deal{
			Index: uint32(d.oidx),
			Deal:  deals[i],
		}
		// sign the deal
		buff, err := distd.MarshalBinary()
		if err != nil {
			return nil, err
		}
		distd.Signature, err = schnorr.Sign(d.suite, d.long, buff)
		//fmt.Println("dkg", d.oidx, d.nidx, " msg = ", hex.EncodeToString(buff)[0:16], "signature = ", hex.EncodeToString(distd.Signature)[0:16], err)
		if err != nil {
			return nil, err
		}

		if i == int(d.nidx) && d.newPresent {
			if d.processed {
				continue
			}
			d.processed = true
			if resp, err := d.ProcessDeal(distd); err != nil {
				panic("dkg: cannot process own deal: " + err.Error())
			} else if resp.Response.Status != vss.StatusApproval {
				panic("dkg: own deal gave a complaint")
			}
			continue
		}
		dd[i] = distd
	}
	return dd, nil
}

// ProcessDeal takes a Deal created by Deals() and stores and verifies it. It
// returns a Response to broadcast to every other participant, including the old
// participants. It returns an error in case the deal has already been stored,
// or if the deal is incorrect (see vss.Verifier.ProcessEncryptedDeal).
func (d *DistKeyGenerator) ProcessDeal(dd *Deal) (*Response, error) {
	if !d.newPresent {
		return nil, errors.New("dkg: unexpected deal for unlisted dealer in new list")
	}
	var pub kyber.Point
	var ok bool
	if d.isResharing {
		pub, ok = getPub(d.c.OldNodes, dd.Index)
	} else {
		pub, ok = getPub(d.c.NewNodes, dd.Index)
	}
	// public key of the dealer
	if !ok {
		return nil, errors.New("dkg: dist deal out of bounds index")
	}

	// verify signature
	buff, err := dd.MarshalBinary()
	if err != nil {
		return nil, err
	}
	if err := schnorr.Verify(d.suite, pub, buff, dd.Signature); err != nil {
		//fmt.Println("dkg", d.nidx, d.oidx, " verify: msg ", hex.EncodeToString(buff)[0:16], "signature = ", hex.EncodeToString(dd.Signature)[0:16])
		return nil, err
	}

	ver, exists := d.verifiers[dd.Index]
	if !exists {
		panic("aieAieaie")
	}

	resp, err := ver.ProcessEncryptedDeal(dd.Deal)
	if err != nil {
		return nil, err
	}

	reject := func() (*Response, error) {
		idx, present := findPub(d.c.NewNodes, pub)
		if present {
			// the dealer is present in both list, so we set its own response
			// (as a verifier) to a complaint since he won't do it himself
			// XXX Nope we should only keep verifiers
			d.verifiers[uint32(dd.Index)].UnsafeSetResponseDKG(uint32(idx), vss.StatusComplaint)
		}
		// indicate to VSS that this dkg's new status is complaint for this
		// deal
		d.verifiers[uint32(dd.Index)].UnsafeSetResponseDKG(uint32(d.nidx), vss.StatusComplaint)
		resp.Status = vss.StatusComplaint
		s, err := schnorr.Sign(d.suite, d.long, resp.Hash(d.suite))
		if err != nil {
			return nil, err
		}
		resp.Signature = s
		return &Response{
			Index:    dd.Index,
			Response: resp,
		}, nil
	}

	if d.isResharing && d.canReceive {
		// verify share integrity wrt to the dist. secret
		dealCommits := ver.Commits()
		// Check that the received committed share is equal to the one we
		// generate from the known public polynomial
		expectedPubShare := d.dpub.Eval(int(dd.Index))
		if !expectedPubShare.V.Equal(dealCommits[0]) {
			return reject()
		}
	}

	// if the dealer in the old list is also present in the new list, then set
	// his response to approval since he won't issue his own response for his
	// own deal
	newIdx, found := findPub(d.c.NewNodes, pub)
	if found {
		d.verifiers[dd.Index].UnsafeSetResponseDKG(uint32(newIdx), vss.StatusApproval)
	}

	return &Response{
		Index:    dd.Index,
		Response: resp,
	}, nil
}

// ProcessResponse takes a response from every other peer.  If the response
// designates the deal of another participant than this dkg, this dkg stores it
// and returns nil with a possible error regarding the validity of the response.
// If the response designates a deal this dkg has issued, then the dkg will process
// the response, and returns a justification.
func (d *DistKeyGenerator) ProcessResponse(resp *Response) (*Justification, error) {
	if d.isResharing && d.canIssue && !d.newPresent {
		return d.processResharingResponse(resp)
	}
	v, ok := d.verifiers[resp.Index]
	if !ok {
		return nil, fmt.Errorf("dkg: responses received for unknown dealer %d", resp.Index)
	}

	if err := v.ProcessResponse(resp.Response); err != nil {
		return nil, err
	}

	myIdx := uint32(d.oidx)
	if !d.canIssue || resp.Index != myIdx {
		// no justification if we dont issue deals or the deal's not from us
		return nil, nil
	}

	j, err := d.dealer.ProcessResponse(resp.Response)
	if err != nil {
		return nil, err
	}
	if j == nil {
		return nil, nil
	}
	if err := v.ProcessJustification(j); err != nil {
		return nil, err
	}

	return &Justification{
		Index:         uint32(d.oidx),
		Justification: j,
	}, nil
}

// special case when an node that is present in the old list but not in the
// new,i.e. leaving the group. This node does not have any verifiers since it
// can't receive shares. This function makes some check on the response and
// returns a justification if the response is invalid.
func (d *DistKeyGenerator) processResharingResponse(resp *Response) (*Justification, error) {
	agg, present := d.oldAggregators[resp.Index]
	if !present {
		agg = vss.NewEmptyAggregator(d.suite, d.c.NewNodes)
		d.oldAggregators[resp.Index] = agg
	}

	err := agg.ProcessResponse(resp.Response)
	if int(resp.Index) != d.oidx {
		return nil, err
	}

	if resp.Response.Status == vss.StatusApproval {
		return nil, nil
	}

	// status is complaint and it is about our deal
	deal, err := d.dealer.PlaintextDeal(int(resp.Response.Index))
	if err != nil {
		return nil, errors.New("dkg: resharing response can't get deal. BUG - REPORT")
	}
	j := &Justification{
		Index: uint32(d.oidx),
		Justification: &vss.Justification{
			SessionID: d.dealer.SessionID(),
			Index:     resp.Response.Index, // good index because of signature check
			Deal:      deal,
		},
	}
	return j, nil
}

// ProcessJustification takes a justification and validates it. It returns an
// error in case the justification is wrong.
func (d *DistKeyGenerator) ProcessJustification(j *Justification) error {
	v, ok := d.verifiers[j.Index]
	if !ok {
		return errors.New("dkg: Justification received but no deal for it")
	}
	return v.ProcessJustification(j.Justification)
}

// SetTimeout triggers the timeout on all verifiers, and thus makes sure
// all verifiers have either responded, or have a StatusComplaint response.
func (d *DistKeyGenerator) SetTimeout() {
	for _, v := range d.verifiers {
		v.SetTimeout()
	}
}

// Certified returns true if a THRESHOLD of deals are certified. To know the
// list of correct receiver, one can call d.QUAL()
// NOTE:
// This method should only be used after a certain timeout - mimicking the
// synchronous assumption of the Pedersen's protocol. One can call
// `FullyCertified()` to check if the DKG is finished and stops it pre-emptively
// if all deals are correct.  If called *before* the timeout, there may be
// inconsistencies in the shares produced. For example, node 1 could have
// aggregated shares from 1, 2, 3 and node 2 could have aggregated shares from
// 2, 3 and 4.
func (d *DistKeyGenerator) Certified() bool {
	if d.isResharing {
		// in resharing case, we have two threshold. Here we want the number of
		// deals to be at least what the old threshold was. (and for each deal,
		// we want the number of approval to be a least what the new threshold
		// is).
		return len(d.QUAL()) >= d.c.OldThreshold
	}

	// in dkg case, the threshold is symmetric -> # verifiers = # dealers
	return len(d.QUAL()) >= d.c.NewThreshold
}

// FullyCertified returns true if *all* deals are certified. This method should
// be called before the timeout occurs, as to pre-emptively stop the DKG
// protocol if it is already finished before the timeout.
func (d *DistKeyGenerator) FullyCertified() bool {
	return len(d.QUAL()) >= len(d.c.OldNodes)
}

// QUAL returns the index in the list of participants that forms the QUALIFIED
// set. Basically, it consists of all valid deals at the end of the protocols.
// It does NOT take into account any malicious share holder which share may have
// been revealed, due to invalid complaint.
// XXX Have a method of retrieving invalid shares ?
func (d *DistKeyGenerator) QUAL() []int {
	var good []int
	if d.isResharing && d.canIssue && !d.newPresent {
		d.oldQualIter(func(i uint32, v *vss.Aggregator) bool {
			good = append(good, int(i))
			return true
		})
		return good
	}
	d.qualIter(func(i uint32, v *vss.Verifier) bool {
		good = append(good, int(i))
		return true
	})
	return good
}

func (d *DistKeyGenerator) isInQUAL(idx uint32) bool {
	var found bool
	d.qualIter(func(i uint32, v *vss.Verifier) bool {
		if i == idx {
			found = true
			return false
		}
		return true
	})
	return found
}

func (d *DistKeyGenerator) qualIter(fn func(idx uint32, v *vss.Verifier) bool) {
	for i, v := range d.verifiers {
		if v.DealCertified() {
			if !fn(i, v) {
				break
			}
		}
	}
}

func (d *DistKeyGenerator) oldQualIter(fn func(idx uint32, v *vss.Aggregator) bool) {
	for i, v := range d.oldAggregators {
		if v.DealCertified() {
			if !fn(i, v) {
				break
			}
		}
	}
}

// DistKeyShare generates the distributed key relative to this receiver.
// It throws an error if something is wrong such as not enough deals received.
// The shared secret can be computed when all deals have been sent and
// basically consists of a public point and a share. The public point is the sum
// of all aggregated individual public commits of each individual secrets.
// the share is evaluated from the global Private Polynomial, basically SUM of
// fj(i) for a receiver i.
func (d *DistKeyGenerator) DistKeyShare() (*DistKeyShare, error) {
	if !d.Certified() {
		return nil, errors.New("dkg: distributed key not certified")
	}
	if !d.canReceive {
		return nil, errors.New("dkg: should not expect to compute any dist. share")
	}

	if d.isResharing {
		return d.resharingKey()
	}

	return d.dkgKey()
}

func (d *DistKeyGenerator) dkgKey() (*DistKeyShare, error) {
	sh := d.suite.Scalar().Zero()
	var pub *share.PubPoly
	var err error
	d.qualIter(func(i uint32, v *vss.Verifier) bool {
		// share of dist. secret = sum of all share received.
		deal := v.Deal()
		s := deal.SecShare.V
		sh = sh.Add(sh, s)
		// Dist. public key = sum of all revealed commitments
		poly := share.NewPubPoly(d.suite, d.suite.Point().Base(), deal.Commitments)
		if pub == nil {
			// first polynomial we see (instead of generating n empty commits)
			pub = poly
			return true
		}
		pub, err = pub.Add(poly)
		return err == nil
	})

	if err != nil {
		return nil, err
	}
	_, commits := pub.Info()

	return &DistKeyShare{
		Commits: commits,
		Share: &share.PriShare{
			I: int(d.nidx),
			V: sh,
		},
		PrivatePoly: d.dealer.PrivatePoly().Coefficients(),
	}, nil

}

func (d *DistKeyGenerator) resharingKey() (*DistKeyShare, error) {
	// only old nodes sends shares
	shares := make([]*share.PriShare, len(d.c.OldNodes))
	coeffs := make([][]kyber.Point, len(d.c.OldNodes))
	d.qualIter(func(i uint32, v *vss.Verifier) bool {
		deal := v.Deal()
		coeffs[int(i)] = deal.Commitments
		// share of dist. secret. Invertion of rows/column
		deal.SecShare.I = int(i)
		shares[int(i)] = deal.SecShare
		return true
	})

	// the private polynomial is generated from the old nodes, thus inheriting
	// the old threshold condition
	priPoly, err := share.RecoverPriPoly(d.suite, shares, d.oldT, len(d.c.OldNodes))
	if err != nil {
		return nil, err
	}
	privateShare := &share.PriShare{
		I: int(d.nidx),
		V: priPoly.Secret(),
	}

	// recover public polynomial by interpolating coefficient-wise all
	// polynomials
	// the new public polynomial must however have "newT" coefficients since it
	// will be held by the new nodes.
	finalCoeffs := make([]kyber.Point, d.newT)
	for i := 0; i < d.newT; i++ {
		tmpCoeffs := make([]*share.PubShare, len(coeffs))
		// take all i-th coefficients
		for j := range coeffs {
			if coeffs[j] == nil {
				continue
			}
			tmpCoeffs[j] = &share.PubShare{I: j, V: coeffs[j][i]}
		}

		// using the old threshold / length because there are at most
		// len(d.c.OldNodes) i-th coefficients since they are the one generating one
		// each, thus using the old threshold.
		coeff, err := share.RecoverCommit(d.suite, tmpCoeffs, d.oldT, len(d.c.OldNodes))
		if err != nil {
			return nil, err
		}
		finalCoeffs[i] = coeff
	}

	// Reconstruct the final public polynomial
	pubPoly := share.NewPubPoly(d.suite, nil, finalCoeffs)

	if !pubPoly.Check(privateShare) {
		return nil, errors.New("dkg: share do not correspond to public polynomial ><")
	}
	return &DistKeyShare{
		Commits:     finalCoeffs,
		Share:       privateShare,
		PrivatePoly: priPoly.Coefficients(),
	}, nil
}

// Verifiers returns the verifiers keeping state of each deals
func (d *DistKeyGenerator) Verifiers() map[uint32]*vss.Verifier {
	return d.verifiers
}

func (d *DistKeyGenerator) initVerifiers(c *Config) error {
	var alreadyTaken = make(map[string]bool)
	verifierList := c.NewNodes
	dealerList := c.OldNodes
	verifiers := make(map[uint32]*vss.Verifier)
	for i, pub := range dealerList {
		if _, exists := alreadyTaken[pub.String()]; exists {
			return errors.New("duplicate public key in NewNodes list")
		}
		alreadyTaken[pub.String()] = true
		ver, err := vss.NewVerifier(c.Suite, c.Longterm, pub, verifierList)
		if err != nil {
			return err
		}
		// set that the number of approval for this deal must be at the given
		// threshold regarding the new nodes. (see config.
		ver.SetThreshold(c.NewThreshold)
		verifiers[uint32(i)] = ver
	}
	d.verifiers = verifiers

	/*// we include some of the old nodes that are not in the new list because the*/
	//// old nodes are generating the deals, so all nodes need to have a verifier
	//// to process those deals (those coming from old nodes not in the new list)
	//for i, pub := range c.OldNodes {
	//if _, exists := alreadyTaken[pub.String()]; exists {
	//continue
	//}
	//ver, err := vss.NewVerifier(c.Suite, c.Longterm, pub, list)
	//if err != nil {
	//return err
	//}
	//// however, if we have a old group disjoint, we will overwrite this
	//// entry. That is the reason we go via public key
	//if _, ok := verifiers[pub]; ok {
	//panic(fmt.Sprintf("we should prevent that index %d", i))
	//}
	//verifiers[pub] = ver
	/*}*/

	return nil
}

//Renew adds the new distributed key share g (with secret 0) to the distributed key share d.
func (d *DistKeyShare) Renew(suite Suite, g *DistKeyShare) (*DistKeyShare, error) {
	// Check G(0) = 0*G.
	if !g.Public().Equal(suite.Point().Base().Mul(suite.Scalar().Zero(), nil)) {
		return nil, errors.New("wrong renewal function")
	}

	// Check whether they have the same index
	if d.Share.I != g.Share.I {
		return nil, errors.New("not the same party")
	}

	newShare := suite.Scalar().Add(d.Share.V, g.Share.V)
	newCommits := make([]kyber.Point, len(d.Commits))
	for i := range newCommits {
		newCommits[i] = suite.Point().Add(d.Commits[i], g.Commits[i])
	}
	return &DistKeyShare{
		Commits: newCommits,
		Share: &share.PriShare{
			I: d.Share.I,
			V: newShare,
		},
	}, nil
}

func getPub(list []kyber.Point, i uint32) (kyber.Point, bool) {
	if i >= uint32(len(list)) {
		return nil, false
	}
	return list[i], true
}

func findPub(list []kyber.Point, toFind kyber.Point) (int, bool) {
	for i, p := range list {
		if p.Equal(toFind) {
			return i, true
		}
	}
	return 0, false
}

func checksDealCertified(i uint32, v *vss.Verifier) bool {
	return v.DealCertified()
}

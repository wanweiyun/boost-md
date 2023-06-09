package server

import (
	"context"
	"fmt"
	"github.com/filecoin-project/boost-gfm/piecestore"
	"github.com/filecoin-project/boost-gfm/retrievalmarket"
	"github.com/filecoin-project/boost-gfm/stores"
	"github.com/ipfs/go-cid"
	"github.com/ipld/go-ipld-prime"
	cidlink "github.com/ipld/go-ipld-prime/linking/cid"
	"github.com/ipld/go-ipld-prime/traversal"
	"github.com/multiformats/go-multihash"
)

// This code is copied directly from
// https://github.com/filecoin-project/go-fil-markets/blob/955fd43fad7da2e68539c257f0c8199a6b0c2a4d/retrievalmarket/impl/provider_pieces.go#L1
// TODO: Create a PR against go-fil-markets to make these methods public,
// so that we can import them from go-fil-markets instead of copying the code here.

// MaxIdentityCIDBytes is the largest identity CID as a PayloadCID that we are
// willing to decode
const MaxIdentityCIDBytes = 2 << 10

// MaxIdentityCIDLinks is the maximum number of links contained within an
// identity CID that we are willing to check for matching pieces
const MaxIdentityCIDLinks = 32

// GetAllPieceInfoForPayload returns all of the pieces containing the requested Payload CID.
// If the Payload CID is an identity CID, then we use getCommonPiecesFromIdentityCidLinks to find
// pieces containing all of the links within that identity CID.
// Note that it is possible to receive a non-nil error as well as a non-zero length PieceInfo slice
// as a return from this function. In that case, there was at least one error encountered querying
// the piece store.
func GetAllPieceInfoForPayload(dagStore stores.DAGStoreWrapper, pieceStore piecestore.PieceStore, payloadCID cid.Cid) ([]piecestore.PieceInfo, error) {
	// Get all pieces that contain the target block
	piecesWithTargetBlock, err := dagStore.GetPiecesContainingBlock(payloadCID)
	if err != nil {
		// this payloadCID may be an identity CID that's in the root of a CAR but
		// not recorded in the index
		var idErr error
		piecesWithTargetBlock, idErr = GetCommonPiecesFromIdentityCidLinks(dagStore.GetPiecesContainingBlock, payloadCID)
		if idErr != nil {
			return []piecestore.PieceInfo{}, idErr
		}
		if len(piecesWithTargetBlock) == 0 {
			return []piecestore.PieceInfo{}, fmt.Errorf("getting pieces for cid %s: %w", payloadCID, err)
		}
	}

	pieces := make([]piecestore.PieceInfo, 0)
	var lastErr error
	for _, pieceWithTargetBlock := range piecesWithTargetBlock {
		// Get the deals for the piece
		pieceInfo, err := pieceStore.GetPieceInfo(pieceWithTargetBlock)
		if err != nil {
			lastErr = err
			continue
		}
		pieces = append(pieces, pieceInfo)
	}

	return pieces, lastErr
}

// GetCommonPiecesFromIdentityCidLinks will inspect a payloadCID and if it has an identity multihash,
// will determine which pieces contain all of the links within the decoded identity multihash block
func GetCommonPiecesFromIdentityCidLinks(piecesWithCid func(c cid.Cid) ([]cid.Cid, error), payloadCID cid.Cid) ([]cid.Cid, error) {
	links, err := LinksFromIdentityCid(payloadCID)
	if err != nil || len(links) == 0 {
		return links, err
	}

	pieces := make([]cid.Cid, 0)
	// for each link, query the dagstore for pieces that contain it
	for i, link := range links {
		piecesWithThisCid, err := piecesWithCid(link)
		if err != nil {
			return nil, fmt.Errorf("getting pieces for identity CID sub-link %s: %w", link, err)
		}
		if len(piecesWithThisCid) == 0 {
			return nil, fmt.Errorf("no pieces for identity CID sub-link %s", link)
		}
		if i == 0 {
			pieces = append(pieces, piecesWithThisCid...)
		} else {
			// after the first, find the intersection between these pieces and the previous ones
			intersection := make([]cid.Cid, 0)
			for _, cj := range piecesWithThisCid {
				for _, ck := range pieces {
					if cj.Equals(ck) {
						intersection = append(intersection, cj)
						break
					}
				}
			}
			pieces = intersection
		}
		if len(pieces) == 0 {
			break
		}
	}

	return pieces, nil
}

// LinksFromIdentityCid will extract zero or more CIDs contained within a valid identity CID.
// If the CID is not an identity CID, an empty list is returned. If the CID is an identity CID and
// cannot be decoded, an error is returned.
func LinksFromIdentityCid(identityCid cid.Cid) ([]cid.Cid, error) {
	if identityCid.Prefix().MhType != multihash.IDENTITY {
		return []cid.Cid{}, nil
	}

	if len(identityCid.Hash()) > MaxIdentityCIDBytes {
		return nil, fmt.Errorf("refusing to decode too-long identity CID (%d bytes)", len(identityCid.Hash()))
	}

	// decode the identity multihash, if possible (i.e. it's valid and we have the right codec loaded)
	decoder, err := cidlink.DefaultLinkSystem().DecoderChooser(cidlink.Link{Cid: identityCid})
	if err != nil {
		return nil, fmt.Errorf("choosing decoder for identity CID %s: %w", identityCid, err)
	}
	mh, err := multihash.Decode(identityCid.Hash())
	if err != nil {
		return nil, fmt.Errorf("decoding identity CID multihash %s: %w", identityCid, err)
	}
	node, err := ipld.Decode(mh.Digest, decoder)
	if err != nil {
		return nil, fmt.Errorf("decoding identity CID %s: %w", identityCid, err)
	}
	links, err := traversal.SelectLinks(node)
	if err != nil {
		return nil, fmt.Errorf("collecting links from identity CID %s: %w", identityCid, err)
	}

	// convert from Link to Cid, handle nested identity CIDs, and dedupe
	resultCids := make([]cid.Cid, 0)
	for _, link_ := range links {
		cids := []cid.Cid{link_.(cidlink.Link).Cid}
		if cids[0].Prefix().MhType == multihash.IDENTITY {
			// nested, recurse
			// (just because you can, it doesn't mean you should, nested identity CIDs are an extra layer of silly)
			cids, err = LinksFromIdentityCid(cids[0])
			if err != nil {
				return nil, err
			}
		}
		for _, c := range cids {
			// dedupe
			var found bool
			for _, rc := range resultCids {
				if rc.Equals(c) {
					found = true
				}
			}
			if !found {
				resultCids = append(resultCids, c)
			}
		}
	}

	if len(resultCids) > MaxIdentityCIDLinks {
		return nil, fmt.Errorf("refusing to process identity CID with too many links (%d)", len(resultCids))
	}

	return resultCids, err
}

func PieceInUnsealedSector(ctx context.Context, sa retrievalmarket.SectorAccessor, pieceInfo piecestore.PieceInfo) bool {
	for _, di := range pieceInfo.Deals {
		isUnsealed, err := sa.IsUnsealed(ctx, di.SectorID, di.Offset.Unpadded(), di.Length.Unpadded())
		if err != nil {
			log.Errorf("failed to find out if sector %d is unsealed, err=%s", di.SectorID, err)
			continue
		}
		if isUnsealed {
			return true
		}
	}

	return false
}

// GetBestPieceInfoMatch will take a list of pieces, and an optional PieceCID from a client, and
// will find the best piece to use for a retrieval. If a specific PieceCID is provided and that
// piece is included in the list of pieces, that is used. Otherwise the first unsealed piece is used
// and if there are no unsealed pieces, the first sealed piece is used.
// Failure to find a matching piece will result in a piecestore.PieceInfoUndefined being returned.
func GetBestPieceInfoMatch(ctx context.Context, sa retrievalmarket.SectorAccessor, pieces []piecestore.PieceInfo, clientPieceCID cid.Cid) (piecestore.PieceInfo, bool) {
	sealedPieceInfo := -1
	// For each piece that contains the target block
	for ii, pieceInfo := range pieces {
		fmt.Printf("[test] ii:%v  pieceInfo:%v clientPieceCID:%v",ii,pieceInfo,clientPieceCID)
		if clientPieceCID.Defined() {
			// If client wants to retrieve the payload from a specific piece, just return that piece.
			if pieceInfo.PieceCID.Equals(clientPieceCID) {
				fmt.Printf("[test] pieceInfo.PieceCID.Equals(clientPieceCID)")
				return pieceInfo, PieceInUnsealedSector(ctx, sa, pieceInfo)
			}
		} else {
			// If client doesn't have a preference for a particular piece, prefer the first piece for
			// which an unsealed sector exists.
			if PieceInUnsealedSector(ctx, sa, pieceInfo) {
				// The piece is in an unsealed sector, so just return it
				fmt.Printf("[test] The piece is in an unsealed sector, so just return it")
				return pieceInfo, true
			}

			if sealedPieceInfo == -1 {
				// The piece is not in an unsealed sector, so save it but keep checking other pieces to see
				// if there is one that is in an unsealed sector, otherwise use the first found sealed piece
				fmt.Printf("[test] The piece is not in an unsealed sector, sealedPieceInfo = ii")
				sealedPieceInfo = ii
			}
		}
	}

	// Found a piece containing the target block, piece is in a sealed sector
	if sealedPieceInfo > -1 {
		fmt.Printf("[test] Found a piece containing the target block, piece is in a sealed sector")
		return pieces[sealedPieceInfo], false
	}

	return piecestore.PieceInfoUndefined, false
}

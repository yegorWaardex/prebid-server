package waardex

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"text/template"

	"github.com/prebid/openrtb/v20/openrtb2"
	"github.com/prebid/prebid-server/v4/adapters"
	"github.com/prebid/prebid-server/v4/config"
	"github.com/prebid/prebid-server/v4/errortypes"
	"github.com/prebid/prebid-server/v4/macros"
	"github.com/prebid/prebid-server/v4/openrtb_ext"
	"github.com/prebid/prebid-server/v4/util/jsonutil"
)

type adapter struct {
	EndpointTemplate *template.Template
}

// MakeRequests sends every impression in a request of its own, once per media type it offers.
// The Waardex exchange runs one auction per request and answers with the winning buyer's
// response alone, so impressions sharing a request would also share a single winner; it also
// accepts exactly one media type per impression.
func (adapter *adapter) MakeRequests(request *openrtb2.BidRequest, _ *adapters.ExtraRequestInfo) ([]*adapters.RequestData, []error) {
	var errs []error
	result := make([]*adapters.RequestData, 0, len(request.Imp))
	for _, imp := range request.Imp {
		bidderExt, waardexExt, err := getImpressionExt(&imp)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if isRewardedInventory(bidderExt) {
			// PBS moves imp.rwdd into imp.ext.prebid for OpenRTB 2.5 bidders, a location the
			// exchange does not read.
			imp.Rwdd = 1
		}
		singleFormatImps := splitByMediaType(imp)
		if len(singleFormatImps) == 0 {
			errs = append(errs, &errortypes.BadInput{
				Message: fmt.Sprintf("imp %s has no supported media type (banner or video)", imp.ID),
			})
			continue
		}
		for _, singleFormatImp := range singleFormatImps {
			bidRequest, err := adapter.buildAdapterRequest(request, waardexExt, singleFormatImp)
			if err != nil {
				errs = append(errs, err)
			} else {
				result = append(result, bidRequest)
			}
		}
	}
	return result, errs
}

// splitByMediaType returns one copy of imp per media type Waardex bids on, each carrying only that media type.
func splitByMediaType(imp openrtb2.Imp) []openrtb2.Imp {
	splitImps := make([]openrtb2.Imp, 0, 2)
	if imp.Banner != nil {
		impCopy := imp
		impCopy.Video = nil
		impCopy.Native = nil
		impCopy.Audio = nil
		splitImps = append(splitImps, impCopy)
	}
	if imp.Video != nil {
		impCopy := imp
		impCopy.Banner = nil
		impCopy.Native = nil
		impCopy.Audio = nil
		splitImps = append(splitImps, impCopy)
	}
	return splitImps
}

func getImpressionExt(imp *openrtb2.Imp) (*adapters.ExtImpBidder, *openrtb_ext.ExtImpWaardex, error) {
	var bidderExt adapters.ExtImpBidder
	if err := jsonutil.Unmarshal(imp.Ext, &bidderExt); err != nil {
		return nil, nil, &errortypes.BadInput{
			Message: fmt.Sprintf("imp.ext is invalid: %s", err.Error()),
		}
	}
	var waardexExt openrtb_ext.ExtImpWaardex
	if err := jsonutil.Unmarshal(bidderExt.Bidder, &waardexExt); err != nil {
		return nil, nil, &errortypes.BadInput{
			Message: fmt.Sprintf("imp.ext.bidder is invalid: %s", err.Error()),
		}
	}
	return &bidderExt, &waardexExt, nil
}

func isRewardedInventory(bidderExt *adapters.ExtImpBidder) bool {
	return bidderExt.Prebid != nil && bidderExt.Prebid.IsRewardedInventory != nil && *bidderExt.Prebid.IsRewardedInventory == 1
}

func (adapter *adapter) buildAdapterRequest(prebidBidRequest *openrtb2.BidRequest, params *openrtb_ext.ExtImpWaardex, imp openrtb2.Imp) (*adapters.RequestData, error) {
	newBidRequest := *prebidBidRequest
	newBidRequest.Imp = []openrtb2.Imp{imp}
	reqJSON, err := json.Marshal(newBidRequest)
	if err != nil {
		return nil, err
	}

	headers := http.Header{}
	headers.Add("Content-Type", "application/json;charset=utf-8")
	headers.Add("Accept", "application/json")
	headers.Add("x-openrtb-version", "2.5")

	url, err := adapter.buildEndpointURL(params)
	if err != nil {
		return nil, err
	}

	return &adapters.RequestData{
		Method:  "POST",
		Uri:     url,
		Body:    reqJSON,
		Headers: headers,
		ImpIDs:  openrtb_ext.GetImpIDs(newBidRequest.Imp)}, nil
}

// Builds endpoint url based on adapter-specific pub settings from imp.ext
func (adapter *adapter) buildEndpointURL(params *openrtb_ext.ExtImpWaardex) (string, error) {
	endpointParams := macros.EndpointTemplateParams{ZoneID: strconv.Itoa(params.ZoneId)}
	return macros.ResolveMacros(adapter.EndpointTemplate, endpointParams)
}

// MakeBids translates Waardex bid response to prebid-server specific format
func (adapter *adapter) MakeBids(internalRequest *openrtb2.BidRequest, externalRequest *adapters.RequestData, response *adapters.ResponseData) (*adapters.BidderResponse, []error) {
	if response.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	if response.StatusCode != http.StatusOK {
		return nil, []error{
			newBadServerResponseError(fmt.Sprintf("Unexpected http status code: %d", response.StatusCode)),
		}
	}
	var bidResp openrtb2.BidResponse
	if err := jsonutil.Unmarshal(response.Body, &bidResp); err != nil {
		return nil, []error{
			newBadServerResponseError(fmt.Sprintf("Bad server response: %v", err)),
		}
	}

	if len(bidResp.SeatBid) != 1 {
		return nil, []error{
			newBadServerResponseError(fmt.Sprintf("Invalid SeatBids count: %d", len(bidResp.SeatBid))),
		}
	}

	seatBid := bidResp.SeatBid[0]
	bidResponse := adapters.NewBidderResponseWithBidsCapacity(len(bidResp.SeatBid[0].Bid))
	if bidResp.Cur != "" {
		bidResponse.Currency = bidResp.Cur
	}
	var errs []error
	for i := 0; i < len(seatBid.Bid); i++ {
		bid := seatBid.Bid[i]
		bidType, err := getMediaTypeForBid(&bid, externalRequest)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		bidResponse.Bids = append(bidResponse.Bids, &adapters.TypedBid{
			Bid:     &bid,
			BidType: bidType,
		})
	}
	return bidResponse, errs
}

// getMediaTypeForBid prefers the bid's own mtype. Many buyers behind the exchange leave it unset;
// such a bid takes the media type of the impression it was requested for.
func getMediaTypeForBid(bid *openrtb2.Bid, externalRequest *adapters.RequestData) (openrtb_ext.BidType, error) {
	switch bid.MType {
	case openrtb2.MarkupBanner:
		return openrtb_ext.BidTypeBanner, nil
	case openrtb2.MarkupVideo:
		return openrtb_ext.BidTypeVideo, nil
	case 0:
		return getRequestedMediaType(externalRequest)
	default:
		return "", &errortypes.BadServerResponse{
			Message: fmt.Sprintf("Unsupported MType %d", bid.MType),
		}
	}
}

// getRequestedMediaType reads the media type of the single banner or video impression every outgoing request carries.
func getRequestedMediaType(externalRequest *adapters.RequestData) (openrtb_ext.BidType, error) {
	var request openrtb2.BidRequest
	if err := jsonutil.Unmarshal(externalRequest.Body, &request); err != nil {
		return "", err
	}
	if len(request.Imp) > 0 && request.Imp[0].Video != nil {
		return openrtb_ext.BidTypeVideo, nil
	}
	return openrtb_ext.BidTypeBanner, nil
}

func newBadServerResponseError(message string) error {
	return &errortypes.BadServerResponse{
		Message: message,
	}
}

// Builder builds a new instance of the waardex adapter for the given bidder with the given config.
func Builder(bidderName openrtb_ext.BidderName, config config.Adapter, server config.Server) (adapters.Bidder, error) {
	urlTemplate, err := template.New("endpointTemplate").Parse(config.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("unable to parse endpoint url template: %v", err)
	}

	bidder := &adapter{
		EndpointTemplate: urlTemplate,
	}
	return bidder, nil
}

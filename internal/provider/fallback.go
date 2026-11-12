package provider

import (
	_ "embed"

	"google.golang.org/protobuf/encoding/protojson"

	sdkpb "github.com/tctien342/pig-cursor-sdk/gen/sdk/v1"
)

// fallbackCatalogJSON is a snapshot of Cursor's public model catalog: the
// offline baseline for `pig --list-models cursor` before any network call.
// Refresh with scripts/gen-fallback-catalog.py. It holds public metadata only.
//
//go:embed fallback_catalog.json
var fallbackCatalogJSON []byte

// FallbackCatalog parses the embedded snapshot.
func FallbackCatalog() ([]*sdkpb.SdkModel, error) {
	return ParseModelItems(fallbackCatalogJSON)
}

// ParseModelItems decodes a JSON array of SdkModel objects, skipping entries it
// cannot read and discarding fields newer bridges may add.
func ParseModelItems(raw []byte) ([]*sdkpb.SdkModel, error) {
	var list []map[string]any
	if err := jsonUnmarshal(raw, &list); err != nil {
		return nil, err
	}
	opts := protojson.UnmarshalOptions{DiscardUnknown: true}
	out := make([]*sdkpb.SdkModel, 0, len(list))
	for _, entry := range list {
		rawEntry, err := jsonMarshal(entry)
		if err != nil {
			continue
		}
		m := &sdkpb.SdkModel{}
		if err := opts.Unmarshal(rawEntry, m); err != nil {
			continue
		}
		if m.GetId() == "" {
			continue
		}
		out = append(out, m)
	}
	return out, nil
}

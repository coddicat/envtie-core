package vercel

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/coddicat/envtie-core/adapters/httpclient"
	"github.com/coddicat/envtie-core/sdk/adapter"
)

const maxBlobName = 70

type blobStore struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Type   string `json:"type"`
	Access string `json:"access"`
	Region string `json:"region"`
	Status string `json:"status"`
}

type blobStoreResponse struct {
	Store blobStore `json:"store"`
}

type blobStoresResponse struct {
	Stores []blobStore `json:"stores"`
}

type blobStoreSecrets struct {
	ReadWriteToken string `json:"rwToken"`
}

func handleBlob(ctx context.Context, r adapter.Request, auth string) (adapter.Response, error) {
	teamID := strings.TrimSpace(r.Config["team_id"])
	name := blobName(strings.TrimSpace(r.Config["name"]))
	switch r.Hook {
	case "read", "validate":
		if r.ExternalID == "" {
			if name == "" {
				return adapter.Response{Absent: true}, nil
			}
			found, err := findBlobByName(ctx, auth, teamID, name)
			if err != nil {
				return adapter.Response{}, err
			}
			if found == nil {
				return adapter.Response{Absent: true}, nil
			}
			return adapter.Response{ExternalID: found.ID, Status: "ready"}, nil
		}
		store, status, err := getBlobStore(ctx, auth, teamID, r.ExternalID)
		if status == http.StatusNotFound {
			return adapter.Response{Absent: true}, nil
		}
		if err != nil {
			return adapter.Response{}, classify(status, err)
		}
		if name != "" && store.Name != name {
			return adapter.Response{}, adapter.NewError("vercel_blob_unmanaged")
		}
		return adapter.Response{ExternalID: store.ID, Status: "ready"}, nil
	case "apply":
		if name == "" {
			return adapter.Response{}, adapter.NewError("vercel_blob_name_required")
		}
		access, region, err := blobCreateConfig(r.Config)
		if err != nil {
			return adapter.Response{}, err
		}
		if existing, err := findBlobByName(ctx, auth, teamID, name); err != nil {
			return adapter.Response{}, err
		} else if existing != nil {
			out, err := blobOutputs(ctx, auth, teamID, existing.ID)
			if err != nil {
				return adapter.Response{}, err
			}
			return adapter.Response{ExternalID: existing.ID, Status: "ready", Outputs: out}, nil
		}
		created, status, err := createBlobStore(ctx, auth, teamID, name, access, region)
		if err != nil {
			return adapter.Response{}, classify(status, err)
		}
		if created.ID == "" {
			return adapter.Response{}, adapter.NewError("vercel_unavailable")
		}
		out, err := blobOutputs(ctx, auth, teamID, created.ID)
		if err != nil {
			return adapter.Response{}, err
		}
		return adapter.Response{ExternalID: created.ID, Status: "created", Outputs: out}, nil
	case "outputs":
		if r.ExternalID == "" {
			return adapter.Response{}, adapter.NewError("vercel_blob_missing")
		}
		store, status, err := getBlobStore(ctx, auth, teamID, r.ExternalID)
		if err != nil {
			return adapter.Response{}, classify(status, err)
		}
		if name != "" && store.Name != name {
			return adapter.Response{}, adapter.NewError("vercel_blob_unmanaged")
		}
		out, err := blobOutputs(ctx, auth, teamID, store.ID)
		if err != nil {
			return adapter.Response{}, err
		}
		return adapter.Response{Outputs: out}, nil
	case "destroy":
		id := r.ExternalID
		if id == "" && name != "" {
			found, err := findBlobByName(ctx, auth, teamID, name)
			if err != nil {
				return adapter.Response{}, err
			}
			if found == nil {
				return adapter.Response{Absent: true}, nil
			}
			id = found.ID
		}
		if id == "" {
			return adapter.Response{Absent: true}, nil
		}
		status, err := httpclient.JSON(ctx, http.MethodDelete, withTeam(apiBase+"/v1/storage/stores/blob/"+url.PathEscape(id), teamID), auth, nil, nil)
		if status == http.StatusNotFound {
			return adapter.Response{Absent: true}, nil
		}
		if err != nil {
			return adapter.Response{}, classify(status, err)
		}
		return adapter.Response{Absent: true}, nil
	default:
		return adapter.Response{}, adapter.NewError("vercel_invalid_request")
	}
}

func blobName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	if len(name) > maxBlobName {
		return name[:maxBlobName]
	}
	return name
}

func blobCreateConfig(cfg map[string]string) (access, region string, err error) {
	access = strings.ToLower(strings.TrimSpace(cfg["access"]))
	if access == "" {
		access = "private"
	}
	switch access {
	case "public", "private":
	default:
		return "", "", adapter.NewError("vercel_blob_access_invalid")
	}
	region = strings.TrimSpace(cfg["region"])
	if region == "" {
		region = "iad1"
	}
	return access, region, nil
}

func blobOutputs(ctx context.Context, auth, teamID, storeID string) (map[string]string, error) {
	secrets, status, err := getBlobSecrets(ctx, auth, teamID, storeID)
	if err != nil {
		return nil, classify(status, err)
	}
	if strings.TrimSpace(secrets.ReadWriteToken) == "" {
		return nil, adapter.NewError("vercel_blob_token_unavailable")
	}
	return map[string]string{
		"BLOB_STORE_ID":         storeID,
		"BLOB_READ_WRITE_TOKEN": secrets.ReadWriteToken,
	}, nil
}

func findBlobByName(ctx context.Context, auth, teamID, name string) (*blobStore, error) {
	list, status, err := listBlobStores(ctx, auth, teamID)
	if err != nil {
		return nil, classify(status, err)
	}
	for i := range list {
		if list[i].Type != "" && list[i].Type != "blob" {
			continue
		}
		if list[i].Name == name {
			return &list[i], nil
		}
	}
	return nil, nil
}

func listBlobStores(ctx context.Context, auth, teamID string) ([]blobStore, int, error) {
	var resp blobStoresResponse
	status, err := httpclient.JSON(ctx, http.MethodGet, withTeam(apiBase+"/v1/storage/stores", teamID), auth, nil, &resp)
	if err != nil {
		return nil, status, err
	}
	out := make([]blobStore, 0, len(resp.Stores))
	for _, store := range resp.Stores {
		if store.Type == "" || store.Type == "blob" {
			out = append(out, store)
		}
	}
	return out, status, nil
}

func getBlobStore(ctx context.Context, auth, teamID, id string) (blobStore, int, error) {
	var resp blobStoreResponse
	status, err := httpclient.JSON(ctx, http.MethodGet, withTeam(apiBase+"/v1/storage/stores/"+url.PathEscape(id), teamID), auth, nil, &resp)
	return resp.Store, status, err
}

func getBlobSecrets(ctx context.Context, auth, teamID, id string) (blobStoreSecrets, int, error) {
	var secrets blobStoreSecrets
	status, err := httpclient.JSON(ctx, http.MethodGet, withTeam(apiBase+"/v1/storage/stores/"+url.PathEscape(id)+"/secrets", teamID), auth, nil, &secrets)
	return secrets, status, err
}

func createBlobStore(ctx context.Context, auth, teamID, name, access, region string) (blobStore, int, error) {
	body := map[string]any{"name": name, "access": access, "region": region}
	var resp blobStoreResponse
	status, err := httpclient.JSON(ctx, http.MethodPost, withTeam(apiBase+"/v1/storage/stores/blob", teamID), auth, body, &resp)
	return resp.Store, status, err
}

package storage

import (
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"path"

	"github.com/aldor007/stow"
	fileStorage "github.com/aldor007/stow/local"
	s3Storage "github.com/aldor007/stow/s3"
	httpStorage "mort/storage/http"

	"mort/object"
	"mort/response"
)

const notFound = "{\"error\":\"item not found\"}"

func Get(obj *object.FileObject) *response.Response {
	key := getKey(obj)
	client, err := getClient(obj)
	if err != nil {
		return response.NewError(503, err)
	}

	item, err := client.Item(key)
	if err != nil {
		if err == stow.ErrNotFound {
			return response.NewBuf(404, []byte(notFound))
		}

		return response.NewError(544, err)
	}

	metadata, err := item.Metadata()
	if err != nil {
		return response.NewError(500, err)
	}

	reader, err := item.Open()
	if err != nil {
		return response.NewError(500, err)
	}

	return prepareResponse(obj, reader, metadata)
}

func Set(obj *object.FileObject, _ http.Header, contentLen int64, body io.ReadCloser) *response.Response {
	client, err := getClient(obj)
	if err != nil {
		return response.NewError(503, err)
	}

	_, err = client.Put(getKey(obj), body, contentLen, nil)

	if err != nil {
		return response.NewError(500, err)
	}

	res := response.NewBuf(200, []byte(""))
	res.SetContentType(mime.TypeByExtension(path.Ext(obj.Key)))
	return res
}

func getClient(obj *object.FileObject) (stow.Container, error) {
	storageCfg := obj.Storage
	var config stow.Config
	var client stow.Location

	switch storageCfg.Kind {
	case "local":
		config = stow.ConfigMap{
			fileStorage.ConfigKeyPath: storageCfg.RootPath,
		}
	case "http":
		headers, _ := json.Marshal(storageCfg.Headers)
		config = stow.ConfigMap{
			httpStorage.ConfigUrl:    storageCfg.Url,
			httpStorage.ConfigHeader: string(headers),
		}
	case "s3":
		config = stow.ConfigMap{
			s3Storage.ConfigAccessKeyID: storageCfg.AccessKey,
			s3Storage.ConfigSecretKey:   storageCfg.SecretAccessKey,
			s3Storage.ConfigRegion:      storageCfg.Region,
			s3Storage.ConfigEndpoint:    storageCfg.Endpoint,
		}

	}

	client, err := stow.Dial(storageCfg.Kind, config)
	if err != nil {
		return nil, err
	}

	// XXX: check if it is ok
	defer client.Close()

	return client.Container(obj.Bucket)
}


func getKey (obj *object.FileObject) string {
	return path.Join(obj.Storage.PathPrefix, obj.Key)
}
func prepareResponse(obj *object.FileObject, stream io.ReadCloser, metadata map[string]interface{}) *response.Response {
	res := response.New(200, stream)

	for k, v := range metadata {
		switch k {
		case  "etag", "last-modified":
			res.Set(k, v.(string))

		}
	}

	if contentType, ok := metadata["content-type"]; ok {
		res.SetContentType(contentType.(string))
	} else {
		res.SetContentType(mime.TypeByExtension(path.Ext(obj.Key)))
	}
	return res
}

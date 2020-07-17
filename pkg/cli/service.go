package cli

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"strings"
	"sync"

	"github.com/dustin/go-humanize"
	jsoniter "github.com/json-iterator/go"
	"github.com/olekukonko/tablewriter"
	"gopkg.in/yaml.v2"

	zotErrors "github.com/anuvu/zot/errors"
)

type ImageSearchService interface {
	getAllImages(ctx context.Context, config searchConfig, username, password string,
		channel chan imageListResult, wg *sync.WaitGroup)
	getImageByName(ctx context.Context, config searchConfig, username, password, imageName string,
		channel chan imageListResult, wg *sync.WaitGroup)
}
type searchService struct{}

func NewImageSearchService() ImageSearchService {
	return searchService{}
}

func (service searchService) getImageByName(ctx context.Context, config searchConfig,
	username, password, imageName string, c chan imageListResult, wg *sync.WaitGroup) {
	defer wg.Done()
	defer close(c)

	var localWg sync.WaitGroup
	p := newSmoothRateLimiter(ctx, &localWg, c)

	localWg.Add(1)

	go p.startRateLimiter()
	localWg.Add(1)

	go getImage(ctx, config, username, password, imageName, c, &localWg, p)

	localWg.Wait()
}

func (service searchService) getAllImages(ctx context.Context, config searchConfig, username, password string,
	c chan imageListResult, wg *sync.WaitGroup) {
	defer wg.Done()
	defer close(c)

	catalog := &catalogResponse{}

	catalogEndPoint, err := combineServerAndEndpointURL(*config.servURL, "/v2/_catalog")
	if err != nil {
		if isContextDone(ctx) {
			return
		}
		c <- imageListResult{"", err}

		return
	}

	_, err = makeGETRequest(catalogEndPoint, username, password, *config.verifyTLS, catalog)
	if err != nil {
		if isContextDone(ctx) {
			return
		}
		c <- imageListResult{"", err}

		return
	}

	var localWg sync.WaitGroup

	p := newSmoothRateLimiter(ctx, &localWg, c)

	localWg.Add(1)

	go p.startRateLimiter()

	for _, repo := range catalog.Repositories {
		localWg.Add(1)

		go getImage(ctx, config, username, password, repo, c, &localWg, p)
	}

	localWg.Wait()
}

func getImage(ctx context.Context, config searchConfig, username, password, imageName string,
	c chan imageListResult, wg *sync.WaitGroup, pool *requestsPool) {
	defer wg.Done()

	tagListEndpoint, err := combineServerAndEndpointURL(*config.servURL, fmt.Sprintf("/v2/%s/tags/list", imageName))
	if err != nil {
		if isContextDone(ctx) {
			return
		}
		c <- imageListResult{"", err}

		return
	}

	tagsList := &tagListResp{}
	_, err = makeGETRequest(tagListEndpoint, username, password, *config.verifyTLS, &tagsList)

	if err != nil {
		if isContextDone(ctx) {
			return
		}
		c <- imageListResult{"", err}

		return
	}

	for _, tag := range tagsList.Tags {
		wg.Add(1)

		go addManifestCallToPool(ctx, config, pool, username, password, imageName, tag, c, wg)
	}
}

func isContextDone(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	default:
		return false
	}
}

func addManifestCallToPool(ctx context.Context, config searchConfig, p *requestsPool, username, password, imageName,
	tagName string, c chan imageListResult, wg *sync.WaitGroup) {
	defer wg.Done()

	resultManifest := manifestResponse{}

	manifestEndpoint, err := combineServerAndEndpointURL(*config.servURL,
		fmt.Sprintf("/v2/%s/manifests/%s", imageName, tagName))
	if err != nil {
		if isContextDone(ctx) {
			return
		}
		c <- imageListResult{"", err}
	}

	job := manifestJob{
		url:          manifestEndpoint,
		username:     username,
		imageName:    imageName,
		password:     password,
		tagName:      tagName,
		manifestResp: resultManifest,
		config:       config,
	}

	wg.Add(1)
	p.submitJob(&job)
}

type tagListResp struct {
	Name string   `json:"name"`
	Tags []string `json:"tags"`
}

type imageStruct struct {
	Name string `json:"name"`
	Tags []tags `json:"tags"`
}
type tags struct {
	Name   string `json:"name"`
	Size   uint64 `json:"size"`
	Digest string `json:"digest"`
}

func (img imageStruct) string(format string) (string, error) {
	switch strings.ToLower(format) {
	case "", "text":
		return img.stringPlainText()
	case "json":
		return img.stringJSON()
	case "yml", "yaml":
		return img.stringYAML()
	default:
		return "", ErrInvalidOutputFormat
	}
}

func (img imageStruct) stringPlainText() (string, error) {
	var builder strings.Builder

	table := getNoBorderTableWriter(&builder)

	for _, tag := range img.Tags {
		imageName := ellipsize(img.Name, imageNameWidth, ellipsis)
		tagName := ellipsize(tag.Name, tagWidth, ellipsis)
		digest := ellipsize(tag.Digest, digestWidth, "")
		size := ellipsize(strings.ReplaceAll(humanize.Bytes(tag.Size), " ", ""), sizeWidth, ellipsis)
		row := []string{imageName,
			tagName,
			digest,
			size,
		}

		table.Append(row)
	}

	table.Render()

	return builder.String(), nil
}

func (img imageStruct) stringJSON() (string, error) {
	var json = jsoniter.ConfigCompatibleWithStandardLibrary
	body, err := json.MarshalIndent(img, "", "  ")

	if err != nil {
		return "", err
	}

	return string(body), nil
}

func (img imageStruct) stringYAML() (string, error) {
	body, err := yaml.Marshal(&img)

	if err != nil {
		return "", err
	}

	return string(body), nil
}

type catalogResponse struct {
	Repositories []string `json:"repositories"`
}

type manifestResponse struct {
	Layers []struct {
		MediaType string `json:"mediaType"`
		Digest    string `json:"digest"`
		Size      uint64 `json:"size"`
	} `json:"layers"`
	Annotations struct {
		WsTychoStackerStackerYaml string `json:"ws.tycho.stacker.stacker_yaml"`
		WsTychoStackerGitVersion  string `json:"ws.tycho.stacker.git_version"`
	} `json:"annotations"`
	Config struct {
		Size      int    `json:"size"`
		Digest    string `json:"digest"`
		MediaType string `json:"mediaType"`
	} `json:"config"`
	SchemaVersion int `json:"schemaVersion"`
}

func combineServerAndEndpointURL(serverURL, endPoint string) (string, error) {
	if !isURL(serverURL) {
		return "", zotErrors.ErrInvalidURL
	}

	newURL, err := url.Parse(serverURL)

	if err != nil {
		return "", zotErrors.ErrInvalidURL
	}

	newURL, _ = newURL.Parse(endPoint)

	return newURL.String(), nil
}

func ellipsize(text string, max int, trailing string) string {
	if len(text) <= max {
		return text
	}

	chopLength := len(trailing)

	return text[:max-chopLength] + trailing
}

func getNoBorderTableWriter(writer io.Writer) *tablewriter.Table {
	table := tablewriter.NewWriter(writer)

	table.SetAutoWrapText(false)
	table.SetAutoFormatHeaders(true)
	table.SetHeaderAlignment(tablewriter.ALIGN_LEFT)
	table.SetAlignment(tablewriter.ALIGN_LEFT)
	table.SetCenterSeparator("")
	table.SetColumnSeparator("")
	table.SetRowSeparator("")
	table.SetHeaderLine(false)
	table.SetBorder(false)
	table.SetTablePadding("  ")
	table.SetNoWhiteSpace(true)
	table.SetColMinWidth(colImageNameIndex, imageNameWidth)
	table.SetColMinWidth(colTagIndex, tagWidth)
	table.SetColMinWidth(colDigestIndex, digestWidth)
	table.SetColMinWidth(colSizeIndex, sizeWidth)

	return table
}

const (
	imageNameWidth = 32
	tagWidth       = 24
	digestWidth    = 8
	sizeWidth      = 8
	ellipsis       = "..."

	colImageNameIndex = 0
	colTagIndex       = 1
	colDigestIndex    = 2
	colSizeIndex      = 3
)
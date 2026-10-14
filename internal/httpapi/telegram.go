package httpapi

import (
	"context"
	"sort"
	"strings"
	"sync"

	"github.com/karilaa-dev/tt-scrap/internal/core"
	"github.com/karilaa-dev/tt-scrap/internal/media"
)

type stringSet map[string]bool

func set(values ...string) stringSet {
	result := make(stringSet, len(values))
	for _, value := range values {
		result[value] = true
	}
	return result
}
func union(base stringSet, values ...string) stringSet {
	result := make(stringSet, len(base)+len(values))
	for key := range base {
		result[key] = true
	}
	for _, key := range values {
		result[key] = true
	}
	return result
}

var commonFields = set(strings.Fields("allow_paid_broadcast business_connection_id callback_query_id caption caption_entities chat_id direct_messages_topic_id disable_notification message_effect_id message_thread_id parse_mode protect_content receiver_user_id reply_markup reply_parameters suggested_post_parameters")...)
var videoFields = union(commonFields, strings.Fields("duration has_spoiler height show_caption_above_media start_timestamp supports_streaming width")...)
var documentFields = union(commonFields, "disable_content_type_detection")
var audioFields = union(commonFields, "duration", "performer", "title")
var photoFields = union(commonFields, "has_spoiler", "show_caption_above_media")
var groupFields = set(strings.Fields("allow_paid_broadcast business_connection_id chat_id direct_messages_topic_id disable_notification message_effect_id message_thread_id protect_content reply_parameters")...)
var carouselFields = union(groupFields, "caption", "caption_entities", "parse_mode")
var carouselDocumentFields = union(carouselFields, "disable_content_type_detection")
var carouselMediaFields = union(carouselFields, "show_caption_above_media", "has_spoiler", "start_timestamp", "supports_streaming")
var sendMethods = map[string]string{"document": "sendDocument", "photo": "sendPhoto", "audio": "sendAudio", "video": "sendVideo"}
var knownFields = func() stringSet {
	result := set("chat_id", "caption_entities")
	for _, list := range [][]string{telegramStringFields, telegramIntegerFields, telegramBoolFields, telegramObjectFields} {
		for _, key := range list {
			result[key] = true
		}
	}
	return result
}()

func fieldsFor(input fields, allowed stringSet) (fields, error) {
	result := make(fields, len(input))
	var invalid []string
	for key, value := range input {
		if value == nil {
			continue
		}
		if knownFields[key] && !allowed[key] {
			invalid = append(invalid, key)
		}
		result[key] = value
	}
	if len(invalid) > 0 {
		sort.Strings(invalid)
		return nil, core.NewError("telegram_parameter_not_supported", "Parameters are invalid for this delivery: "+strings.Join(invalid, ", "), 422)
	}
	return result, nil
}
func setDefaults(target, provided, values fields) {
	for key, value := range values {
		if _, ok := provided[key]; !ok && value != nil {
			target[key] = value
		}
	}
}
func take(target fields, keys ...string) fields {
	result := fields{}
	for _, key := range keys {
		if value, ok := target[key]; ok {
			result[key] = value
			delete(target, key)
		}
	}
	return result
}
func copyFields(input fields) fields {
	output := make(fields, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}
func mergeFields(target, source fields) {
	for key, value := range source {
		target[key] = value
	}
}

type deliveryItem struct {
	Descriptor core.AssetDescriptor
	Kind       string
	Cover      *core.AssetDescriptor
	Asset      core.AssetContext
}
type preparedItem struct {
	Media     media.Prepared
	Thumbnail *media.Prepared
	Kind      string
	Owned     []*core.Downloaded
}

func (p *preparedItem) cleanup() {
	p.Media.Cleanup()
	if p.Thumbnail != nil {
		p.Thumbnail.Cleanup()
	}
	for _, asset := range p.Owned {
		asset.Cleanup()
	}
}

type telegramService struct {
	settings   core.Config
	tiktok     core.TikTokService
	instagram  core.InstagramService
	assets     core.AssetReader
	downloader core.Downloader
	client     *telegramClient
	images     *media.Processor
	pipelines  *core.Limiter
}

func newTelegramService(settings core.Config, dependencies Dependencies) *telegramService {
	return &telegramService{settings: settings, tiktok: dependencies.TikTok, instagram: dependencies.Instagram, assets: dependencies.Assets, downloader: dependencies.Downloader, client: newTelegramClient(settings, dependencies.HTTPClient), images: media.NewProcessor(settings.ImageConcurrency, settings.TempDir), pipelines: core.NewLimiter(settings.TelegramPipelineConcurrency, settings.MaxQueuedRequests)}
}

func (s *telegramService) deliver(ctx context.Context, request telegramRequest, instagram bool) (result deliveryResult, err error) {
	event := &DeliveryEvent{FailedStage: "admission"}
	defer func() {
		if result.Event == nil {
			event.finish(err)
			result.Event = event
		}
	}()
	if s.settings.TelegramBotToken == "" {
		return deliveryResult{}, core.NewError("service_not_configured", "Telegram delivery is not configured", 503)
	}
	release, acquireErr := s.pipelines.Acquire(ctx)
	if acquireErr != nil {
		return deliveryResult{}, acquireErr
	}
	defer release()
	event.FailedStage = "extraction"
	if instagram {
		return s.deliverInstagram(ctx, request)
	}
	var extraction core.TikTokExtraction
	if request.Selector == "extraction_id" {
		extraction, err = s.tiktok.GetExtraction(ctx, request.ExtractionID)
	} else if request.URL != "" {
		extraction, err = s.tiktok.ExtractURL(ctx, request.URL, request.Refresh)
	}
	if err != nil {
		return deliveryResult{}, err
	}
	if request.Delivery == "audio" {
		music := extraction.Music
		if music == nil || music.Audio == nil {
			id := request.VideoID
			if id == "" {
				id = extraction.SourceID
			}
			fetched, err := s.tiktok.ExtractMusic(ctx, id, request.Refresh)
			if err != nil {
				return deliveryResult{}, err
			}
			music = &core.MusicMetadata{Title: fetched.Title, Author: fetched.Author, DurationSeconds: fetched.DurationSeconds, Cover: fetched.Cover, Audio: &fetched.Audio}
		}
		parameters, err := fieldsFor(request.Telegram, audioFields)
		if err != nil {
			return deliveryResult{}, err
		}
		setDefaults(parameters, request.Telegram, fields{"duration": music.DurationSeconds, "title": music.Title, "performer": music.Author})
		return s.send(ctx, []deliveryItem{{Descriptor: *music.Audio, Kind: "audio", Cover: music.Cover}}, parameters, false, false)
	}
	if len(extraction.Media) == 0 {
		return deliveryResult{}, core.NewError("upstream_extraction_error", "TikTok response contained no media", 502)
	}
	document := request.Delivery == "document"
	allowed := photoFields
	if len(extraction.Media) > 1 {
		allowed = groupFields
	} else if document {
		allowed = documentFields
	} else if extraction.ContentType == "video" {
		allowed = videoFields
	}
	parameters, err := fieldsFor(request.Telegram, allowed)
	if err != nil {
		return deliveryResult{}, err
	}
	if len(extraction.Media) == 1 && document {
		setDefaults(parameters, request.Telegram, fields{"disable_content_type_detection": true})
	}
	if !document && extraction.ContentType == "video" {
		defaults := fields{"supports_streaming": true}
		if extraction.Width != nil {
			defaults["width"] = *extraction.Width
		}
		if extraction.Height != nil {
			defaults["height"] = *extraction.Height
		}
		if extraction.DurationSeconds != nil {
			defaults["duration"] = *extraction.DurationSeconds
		}
		setDefaults(parameters, request.Telegram, defaults)
	}
	items := make([]deliveryItem, len(extraction.Media))
	for i, descriptor := range extraction.Media {
		items[i] = deliveryItem{Descriptor: descriptor, Kind: "image"}
		if extraction.ContentType == "video" {
			items[i].Kind = "video"
			items[i].Cover = extraction.Cover
		}
	}
	return s.send(ctx, items, parameters, document, false)
}

func (s *telegramService) deliverInstagram(ctx context.Context, request telegramRequest) (deliveryResult, error) {
	var extraction core.InstagramExtraction
	var err error
	if request.Selector == "extraction_id" {
		extraction, err = s.instagram.GetExtraction(ctx, request.ExtractionID)
	} else {
		extraction, err = s.instagram.ExtractURL(ctx, request.URL, request.Refresh)
	}
	if err != nil {
		return deliveryResult{}, err
	}
	if len(extraction.Media) == 0 {
		return deliveryResult{}, core.NewError("upstream_extraction_error", "Instagram response contained no media", 502)
	}
	document, multiple := request.Delivery == "document", len(extraction.Media) > 1
	allowed := photoFields
	if multiple {
		allowed = carouselMediaFields
		if document {
			allowed = carouselDocumentFields
		}
	} else if document {
		allowed = documentFields
	} else if extraction.Media[0].MediaType == "video" {
		allowed = videoFields
	}
	parameters, err := fieldsFor(request.Telegram, allowed)
	if err != nil {
		return deliveryResult{}, err
	}
	if document {
		setDefaults(parameters, request.Telegram, fields{"disable_content_type_detection": true})
	} else if !multiple && extraction.Media[0].MediaType == "video" {
		setDefaults(parameters, request.Telegram, fields{"supports_streaming": true})
	}
	items := make([]deliveryItem, len(extraction.Media))
	for i, item := range extraction.Media {
		items[i] = deliveryItem{Descriptor: item.Asset, Kind: item.MediaType, Cover: item.Thumbnail}
	}
	return s.send(ctx, items, parameters, document, true)
}

func (s *telegramService) prepare(ctx context.Context, item deliveryItem, document bool, download core.DownloadFunc) (result preparedItem, err error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	result.Kind = item.Kind
	if item.Cover != nil && !document && item.Kind != "image" && s.settings.ThumbnailWait > 0 {
		type thumbnailResult struct {
			image *media.Prepared
			asset *core.Downloaded
		}
		thumbnail := make(chan thumbnailResult, 1)
		go func() {
			reply := thumbnailResult{}
			defer func() { thumbnail <- reply }()
			thumbnailContext, cancel := context.WithTimeout(ctx, s.settings.ThumbnailWait)
			defer cancel()
			assetContext, err := s.assets.GetAsset(item.Cover.AssetID)
			if err != nil {
				return
			}
			asset, err := download(thumbnailContext, assetContext, 0)
			if err != nil {
				return
			}
			reply.asset = asset
			converted, err := s.images.Prepare(thumbnailContext, asset.Path, item.Descriptor.AssetID+"_thumbnail.jpg", true)
			if err != nil {
				return
			}
			if thumbnailContext.Err() != nil {
				converted.Cleanup()
				return
			}
			reply.image = &converted
		}()
		defer func() {
			if err != nil {
				cancel()
			}
			reply := <-thumbnail
			result.Thumbnail = reply.image
			if reply.asset != nil {
				result.Owned = append(result.Owned, reply.asset)
			}
		}()
	}
	limit := s.settings.TelegramMaxBytes
	if item.Kind == "image" {
		limit = 0
	}
	asset, err := download(ctx, item.Asset, limit)
	if err != nil {
		return result, err
	}
	result.Owned = append(result.Owned, asset)
	result.Media = media.Prepared{Path: asset.Path, Filename: media.FilenameForType(item.Descriptor.Filename, asset.ContentType), ContentType: asset.ContentType, Size: asset.Size}
	if item.Kind == "image" && !document {
		converted, err := s.images.Prepare(ctx, asset.Path, item.Descriptor.Filename, false)
		if err != nil {
			if ctx.Err() != nil {
				return result, ctx.Err()
			}
			return result, core.NewError("image_conversion_failed", "Unsupported or corrupt Telegram photo", 502)
		}
		result.Media = converted
	}
	return result, nil
}

func (s *telegramService) prepareItems(ctx context.Context, items []deliveryItem, indices []int, prepared []preparedItem, document bool, download core.DownloadFunc) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	workers := min(len(indices), max(1, s.settings.TelegramPreparationConcurrency))
	var group sync.WaitGroup
	var once sync.Once
	var failure error
	for worker := range workers {
		group.Go(func() {
			for offset := worker; offset < len(indices); offset += workers {
				if ctx.Err() != nil {
					return
				}
				i := indices[offset]
				value, err := s.prepare(ctx, items[i], document, download)
				prepared[i] = value
				if err != nil {
					once.Do(func() { failure = err; cancel() })
					return
				}
			}
		})
	}
	group.Wait()
	if failure != nil {
		return failure
	}
	return ctx.Err()
}

func (s *telegramService) send(ctx context.Context, items []deliveryItem, parameters fields, document, instagram bool) (result deliveryResult, err error) {
	event := &DeliveryEvent{FailedStage: "preparation", Batches: (len(items) + 9) / 10}
	defer func() {
		event.finish(err)
		result.Event = event
	}()
	prepared := make([]preparedItem, len(items))
	defer func() {
		for i := range prepared {
			prepared[i].cleanup()
		}
	}()
	groups := make(map[string][]int)
	var identities []string
	for i := range items {
		asset, err := s.assets.GetAsset(items[i].Descriptor.AssetID)
		if err != nil {
			return deliveryResult{}, err
		}
		items[i].Asset = asset
		if _, ok := groups[asset.IdentityID]; !ok {
			identities = append(identities, asset.IdentityID)
		}
		groups[asset.IdentityID] = append(groups[asset.IdentityID], i)
	}
	// An extraction normally has one identity. Never hold multiple identity
	// leases at once, even if cached assets originate from different extractions.
	for _, id := range identities {
		err := s.downloader.WithIdentity(ctx, id, func(download core.DownloadFunc) error {
			indices := groups[id]
			// Recovery reruns preparation only; uploads happen after the lease closes.
			for _, i := range indices {
				prepared[i].cleanup()
				prepared[i] = preparedItem{}
			}
			return s.prepareItems(ctx, items, indices, prepared, document, download)
		})
		if err != nil {
			return deliveryResult{}, err
		}
	}
	for _, item := range prepared {
		if s.settings.TelegramMaxBytes > 0 && item.Media.Size > s.settings.TelegramMaxBytes {
			return deliveryResult{}, core.NewError("asset_too_large", "Telegram upload exceeds TELEGRAM_UPLOAD_MAX_MB", 413)
		}
	}
	if len(prepared) == 0 {
		return deliveryResult{}, core.NewError("upstream_extraction_error", "No media could be prepared", 502)
	}
	event.FailedStage = "upload"
	if len(prepared) == 1 {
		first := prepared[0]
		field := first.Kind
		if document {
			field = "document"
		} else if field == "image" {
			field = "photo"
		}
		method := sendMethods[field]
		uploads := []upload{{Prepared: first.Media, Field: field + "_file"}}
		parameters[field] = "attach://" + field + "_file"
		if first.Thumbnail != nil {
			parameters["thumbnail"] = "attach://thumbnail_file"
			if field == "video" {
				parameters["cover"] = "attach://thumbnail_file"
			}
			uploads = append(uploads, upload{Prepared: *first.Thumbnail, Field: "thumbnail_file"})
		}
		event.Attempts++
		call, err := s.client.call(ctx, method, parameters, uploads)
		if err != nil {
			return deliveryResult{}, err
		}
		event.record(call)
		return telegramResponse([]telegramCall{call})
	}
	firstFields := take(parameters, "caption", "caption_entities", "parse_mode", "show_caption_above_media")
	allFields := take(parameters, "has_spoiler")
	perVideoFields := take(parameters, "start_timestamp", "supports_streaming")
	detection := parameters["disable_content_type_detection"]
	if detection == nil {
		detection = true
	}
	delete(parameters, "disable_content_type_detection")
	calls := make([]telegramCall, 0, (len(prepared)+9)/10)
	for batchIndex, batch := range albumBatches(prepared) {
		batchFields := copyFields(parameters)
		if batchIndex > 0 {
			delete(batchFields, "reply_parameters")
		}
		uploads := make([]upload, 0, len(batch)*2)
		payload := make([]fields, 0, len(batch))
		for itemIndex, item := range batch {
			attach := attachmentName("media", batchIndex, itemIndex)
			kind := item.Kind
			if document {
				kind = "document"
			} else if kind == "image" {
				kind = "photo"
			}
			value := fields{"type": kind, "media": "attach://" + attach}
			if instagram && batchIndex == 0 && itemIndex == 0 {
				mergeFields(value, firstFields)
			}
			if document {
				value["disable_content_type_detection"] = detection
			} else if instagram {
				mergeFields(value, allFields)
				if item.Kind == "video" {
					value["supports_streaming"] = true
					mergeFields(value, perVideoFields)
				}
			}
			uploads = append(uploads, upload{Prepared: item.Media, Field: attach})
			if !document && item.Thumbnail != nil {
				name := attachmentName("thumbnail", batchIndex, itemIndex)
				value["thumbnail"] = "attach://" + name
				value["cover"] = "attach://" + name
				uploads = append(uploads, upload{Prepared: *item.Thumbnail, Field: name})
			}
			payload = append(payload, value)
		}
		batchFields["media"] = payload
		event.Attempts++
		call, err := s.client.call(ctx, "sendMediaGroup", batchFields, uploads)
		if err != nil {
			return deliveryResult{}, err
		}
		event.record(call)
		calls = append(calls, call)
		if !call.OK {
			break
		}
	}
	return telegramResponse(calls)
}

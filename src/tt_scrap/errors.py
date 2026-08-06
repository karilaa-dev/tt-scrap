"""Stable service error hierarchy."""

from __future__ import annotations


class ScraperError(Exception):
    code = "internal_error"
    status_code = 500


class AuthenticationError(ScraperError):
    code = "authentication_required"
    status_code = 401


class InvalidLinkError(ScraperError):
    code = "invalid_link"
    status_code = 400


class ContentDeletedError(ScraperError):
    code = "content_deleted"
    status_code = 404


class ContentPrivateError(ScraperError):
    code = "content_private"
    status_code = 403


class RateLimitError(ScraperError):
    code = "upstream_rate_limited"
    status_code = 429


class RegionBlockedError(ScraperError):
    code = "region_blocked"
    status_code = 451


class ContentTooLongError(ScraperError):
    code = "content_too_long"
    status_code = 413


class NetworkError(ScraperError):
    code = "upstream_network_error"
    status_code = 502


class ExtractionError(ScraperError):
    code = "upstream_extraction_error"
    status_code = 502


class UpstreamTimeoutError(ScraperError):
    code = "upstream_timeout"
    status_code = 504


class ConfigurationError(ScraperError):
    code = "service_not_configured"
    status_code = 503


class AssetExpiredError(ScraperError):
    code = "asset_not_found_or_expired"
    status_code = 404


class AssetTooLargeError(ScraperError):
    code = "asset_too_large"
    status_code = 413


class ExtractionExpiredError(ScraperError):
    code = "extraction_not_found_or_expired"
    status_code = 404


class UnsupportedDeliveryError(ScraperError):
    code = "unsupported_delivery"
    status_code = 422


class TelegramParameterError(ScraperError):
    code = "telegram_parameter_not_supported"
    status_code = 422


class ImageConversionError(ScraperError):
    code = "image_conversion_failed"
    status_code = 502


class TelegramNetworkError(ScraperError):
    code = "telegram_network_error"
    status_code = 502


class TelegramTimeoutError(ScraperError):
    code = "telegram_timeout"
    status_code = 504

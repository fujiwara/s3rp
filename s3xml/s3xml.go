// Package s3xml holds the XML request and response bodies of the S3 API and
// the helper to render them. The S3 API speaks XML, so any service
// implementing it needs these wire types; they carry no proxy logic.
package s3xml

import (
	"encoding/xml"
	"fmt"
	"net/http"
	"time"
)

// Namespace is the XML namespace of S3 API documents.
const Namespace = "http://s3.amazonaws.com/doc/2006-03-01/"

// FormatTime formats a time the way S3 does in XML responses.
func FormatTime(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

// ListBucketResult is the response of ListObjectsV2.
type ListBucketResult struct {
	XMLName               xml.Name       `xml:"ListBucketResult"`
	XMLNS                 string         `xml:"xmlns,attr"`
	Name                  string         `xml:"Name"`
	Prefix                string         `xml:"Prefix"`
	StartAfter            string         `xml:"StartAfter,omitempty"`
	ContinuationToken     string         `xml:"ContinuationToken,omitempty"`
	NextContinuationToken string         `xml:"NextContinuationToken,omitempty"`
	KeyCount              int32          `xml:"KeyCount"`
	MaxKeys               int32          `xml:"MaxKeys"`
	Delimiter             string         `xml:"Delimiter,omitempty"`
	EncodingType          string         `xml:"EncodingType,omitempty"`
	IsTruncated           bool           `xml:"IsTruncated"`
	Contents              []Object       `xml:"Contents"`
	CommonPrefixes        []CommonPrefix `xml:"CommonPrefixes"`
}

type Object struct {
	Key          string `xml:"Key"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	Size         int64  `xml:"Size"`
	StorageClass string `xml:"StorageClass,omitempty"`
	Owner        *Owner `xml:"Owner,omitempty"`
}

type CommonPrefix struct {
	Prefix string `xml:"Prefix"`
}

type Owner struct {
	ID          string `xml:"ID"`
	DisplayName string `xml:"DisplayName"`
}

// ListBucketResultV1 is the response of ListObjects (version 1).
type ListBucketResultV1 struct {
	XMLName        xml.Name       `xml:"ListBucketResult"`
	XMLNS          string         `xml:"xmlns,attr"`
	Name           string         `xml:"Name"`
	Prefix         string         `xml:"Prefix"`
	Marker         string         `xml:"Marker"`
	NextMarker     string         `xml:"NextMarker,omitempty"`
	MaxKeys        int32          `xml:"MaxKeys"`
	Delimiter      string         `xml:"Delimiter,omitempty"`
	EncodingType   string         `xml:"EncodingType,omitempty"`
	IsTruncated    bool           `xml:"IsTruncated"`
	Contents       []Object       `xml:"Contents"`
	CommonPrefixes []CommonPrefix `xml:"CommonPrefixes"`
}

// LocationConstraint is the response of GetBucketLocation.
type LocationConstraint struct {
	XMLName xml.Name `xml:"LocationConstraint"`
	XMLNS   string   `xml:"xmlns,attr"`
	Value   string   `xml:",chardata"`
}

// DeleteRequest is the request body of DeleteObjects (Multi-Object Delete).
type DeleteRequest struct {
	XMLName xml.Name              `xml:"Delete"`
	Quiet   bool                  `xml:"Quiet"`
	Objects []DeleteRequestObject `xml:"Object"`
}

type DeleteRequestObject struct {
	Key       string `xml:"Key"`
	VersionID string `xml:"VersionId"`
}

// DeleteResult is the response of DeleteObjects.
type DeleteResult struct {
	XMLName xml.Name        `xml:"DeleteResult"`
	XMLNS   string          `xml:"xmlns,attr"`
	Deleted []DeletedObject `xml:"Deleted"`
	Errors  []DeleteError   `xml:"Error"`
}

type DeletedObject struct {
	Key                   string `xml:"Key"`
	VersionID             string `xml:"VersionId,omitempty"`
	DeleteMarker          bool   `xml:"DeleteMarker,omitempty"`
	DeleteMarkerVersionID string `xml:"DeleteMarkerVersionId,omitempty"`
}

type DeleteError struct {
	Key       string `xml:"Key"`
	VersionID string `xml:"VersionId,omitempty"`
	Code      string `xml:"Code"`
	Message   string `xml:"Message"`
}

// CopyObjectResult is the response of CopyObject.
type CopyObjectResult struct {
	XMLName      xml.Name `xml:"CopyObjectResult"`
	XMLNS        string   `xml:"xmlns,attr"`
	ETag         string   `xml:"ETag"`
	LastModified string   `xml:"LastModified,omitempty"`
}

// CopyPartResult is the response of UploadPartCopy.
type CopyPartResult struct {
	XMLName      xml.Name `xml:"CopyPartResult"`
	XMLNS        string   `xml:"xmlns,attr"`
	ETag         string   `xml:"ETag"`
	LastModified string   `xml:"LastModified,omitempty"`
}

// InitiateMultipartUploadResult is the response of CreateMultipartUpload.
type InitiateMultipartUploadResult struct {
	XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
	XMLNS    string   `xml:"xmlns,attr"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	UploadID string   `xml:"UploadId"`
}

// CompleteMultipartUploadResult is the response of CompleteMultipartUpload.
type CompleteMultipartUploadResult struct {
	XMLName           xml.Name `xml:"CompleteMultipartUploadResult"`
	XMLNS             string   `xml:"xmlns,attr"`
	Location          string   `xml:"Location"`
	Bucket            string   `xml:"Bucket"`
	Key               string   `xml:"Key"`
	ETag              string   `xml:"ETag"`
	ChecksumCRC32     string   `xml:"ChecksumCRC32,omitempty"`
	ChecksumCRC32C    string   `xml:"ChecksumCRC32C,omitempty"`
	ChecksumCRC64NVME string   `xml:"ChecksumCRC64NVME,omitempty"`
	ChecksumSHA1      string   `xml:"ChecksumSHA1,omitempty"`
	ChecksumSHA256    string   `xml:"ChecksumSHA256,omitempty"`
	ChecksumType      string   `xml:"ChecksumType,omitempty"`
}

// CompleteMultipartUploadRequest is the request body of CompleteMultipartUpload.
type CompleteMultipartUploadRequest struct {
	XMLName xml.Name       `xml:"CompleteMultipartUpload"`
	Parts   []CompletePart `xml:"Part"`
}

type CompletePart struct {
	PartNumber        int32  `xml:"PartNumber"`
	ETag              string `xml:"ETag"`
	ChecksumCRC32     string `xml:"ChecksumCRC32,omitempty"`
	ChecksumCRC32C    string `xml:"ChecksumCRC32C,omitempty"`
	ChecksumCRC64NVME string `xml:"ChecksumCRC64NVME,omitempty"`
	ChecksumSHA1      string `xml:"ChecksumSHA1,omitempty"`
	ChecksumSHA256    string `xml:"ChecksumSHA256,omitempty"`
}

// ListPartsResult is the response of ListParts.
type ListPartsResult struct {
	XMLName              xml.Name `xml:"ListPartsResult"`
	XMLNS                string   `xml:"xmlns,attr"`
	Bucket               string   `xml:"Bucket"`
	Key                  string   `xml:"Key"`
	UploadID             string   `xml:"UploadId"`
	PartNumberMarker     string   `xml:"PartNumberMarker,omitempty"`
	NextPartNumberMarker string   `xml:"NextPartNumberMarker,omitempty"`
	MaxParts             int32    `xml:"MaxParts"`
	IsTruncated          bool     `xml:"IsTruncated"`
	Parts                []Part   `xml:"Part"`
	Initiator            *Owner   `xml:"Initiator,omitempty"`
	Owner                *Owner   `xml:"Owner,omitempty"`
	StorageClass         string   `xml:"StorageClass,omitempty"`
}

type Part struct {
	PartNumber   int32  `xml:"PartNumber"`
	LastModified string `xml:"LastModified,omitempty"`
	ETag         string `xml:"ETag"`
	Size         int64  `xml:"Size"`
}

// ListMultipartUploadsResult is the response of ListMultipartUploads.
type ListMultipartUploadsResult struct {
	XMLName            xml.Name       `xml:"ListMultipartUploadsResult"`
	XMLNS              string         `xml:"xmlns,attr"`
	Bucket             string         `xml:"Bucket"`
	KeyMarker          string         `xml:"KeyMarker,omitempty"`
	UploadIDMarker     string         `xml:"UploadIdMarker,omitempty"`
	NextKeyMarker      string         `xml:"NextKeyMarker,omitempty"`
	NextUploadIDMarker string         `xml:"NextUploadIdMarker,omitempty"`
	Delimiter          string         `xml:"Delimiter,omitempty"`
	Prefix             string         `xml:"Prefix,omitempty"`
	MaxUploads         int32          `xml:"MaxUploads"`
	IsTruncated        bool           `xml:"IsTruncated"`
	Uploads            []Upload       `xml:"Upload"`
	CommonPrefixes     []CommonPrefix `xml:"CommonPrefixes"`
}

type Upload struct {
	Key          string `xml:"Key"`
	UploadID     string `xml:"UploadId"`
	Initiator    *Owner `xml:"Initiator,omitempty"`
	Owner        *Owner `xml:"Owner,omitempty"`
	StorageClass string `xml:"StorageClass,omitempty"`
	Initiated    string `xml:"Initiated,omitempty"`
}

// VersioningConfiguration is the request and response body of
// PutBucketVersioning / GetBucketVersioning.
type VersioningConfiguration struct {
	XMLName xml.Name `xml:"VersioningConfiguration"`
	XMLNS   string   `xml:"xmlns,attr,omitempty"`
	Status  string   `xml:"Status,omitempty"`
}

// ListVersionsResult is the response of ListObjectVersions.
type ListVersionsResult struct {
	XMLName             xml.Name            `xml:"ListVersionsResult"`
	XMLNS               string              `xml:"xmlns,attr"`
	Name                string              `xml:"Name"`
	Prefix              string              `xml:"Prefix"`
	KeyMarker           string              `xml:"KeyMarker"`
	VersionIDMarker     string              `xml:"VersionIdMarker"`
	NextKeyMarker       string              `xml:"NextKeyMarker,omitempty"`
	NextVersionIDMarker string              `xml:"NextVersionIdMarker,omitempty"`
	MaxKeys             int32               `xml:"MaxKeys"`
	Delimiter           string              `xml:"Delimiter,omitempty"`
	EncodingType        string              `xml:"EncodingType,omitempty"`
	IsTruncated         bool                `xml:"IsTruncated"`
	Versions            []ObjectVersion     `xml:"Version"`
	DeleteMarkers       []DeleteMarkerEntry `xml:"DeleteMarker"`
	CommonPrefixes      []CommonPrefix      `xml:"CommonPrefixes"`
}

type ObjectVersion struct {
	Key          string `xml:"Key"`
	VersionID    string `xml:"VersionId"`
	IsLatest     bool   `xml:"IsLatest"`
	LastModified string `xml:"LastModified,omitempty"`
	ETag         string `xml:"ETag"`
	Size         int64  `xml:"Size"`
	StorageClass string `xml:"StorageClass,omitempty"`
	Owner        *Owner `xml:"Owner,omitempty"`
}

type DeleteMarkerEntry struct {
	Key          string `xml:"Key"`
	VersionID    string `xml:"VersionId"`
	IsLatest     bool   `xml:"IsLatest"`
	LastModified string `xml:"LastModified,omitempty"`
	Owner        *Owner `xml:"Owner,omitempty"`
}

// ObjectLockConfiguration is the request/response body of
// Put/GetObjectLockConfiguration.
type ObjectLockConfiguration struct {
	XMLName           xml.Name        `xml:"ObjectLockConfiguration"`
	XMLNS             string          `xml:"xmlns,attr,omitempty"`
	ObjectLockEnabled string          `xml:"ObjectLockEnabled,omitempty"`
	Rule              *ObjectLockRule `xml:"Rule,omitempty"`
}

type ObjectLockRule struct {
	DefaultRetention *DefaultRetention `xml:"DefaultRetention,omitempty"`
}

type DefaultRetention struct {
	Mode  string `xml:"Mode,omitempty"`
	Days  int32  `xml:"Days,omitempty"`
	Years int32  `xml:"Years,omitempty"`
}

// ObjectLockRetention is the request/response body of
// Put/GetObjectRetention.
type ObjectLockRetention struct {
	XMLName         xml.Name `xml:"Retention"`
	XMLNS           string   `xml:"xmlns,attr,omitempty"`
	Mode            string   `xml:"Mode,omitempty"`
	RetainUntilDate string   `xml:"RetainUntilDate,omitempty"`
}

// ObjectLockLegalHold is the request/response body of
// Put/GetObjectLegalHold.
type ObjectLockLegalHold struct {
	XMLName xml.Name `xml:"LegalHold"`
	XMLNS   string   `xml:"xmlns,attr,omitempty"`
	Status  string   `xml:"Status,omitempty"`
}

// Tagging is the request and response body of PutObjectTagging /
// GetObjectTagging.
type Tagging struct {
	XMLName xml.Name `xml:"Tagging"`
	XMLNS   string   `xml:"xmlns,attr,omitempty"`
	TagSet  TagSet   `xml:"TagSet"`
}

type TagSet struct {
	Tags []Tag `xml:"Tag"`
}

type Tag struct {
	Key   string `xml:"Key"`
	Value string `xml:"Value"`
}

// CORSConfiguration is the response of GetBucketCors.
type CORSConfiguration struct {
	XMLName xml.Name      `xml:"CORSConfiguration"`
	XMLNS   string        `xml:"xmlns,attr"`
	Rules   []CORSRuleXML `xml:"CORSRule"`
}

type CORSRuleXML struct {
	AllowedOrigin []string `xml:"AllowedOrigin"`
	AllowedMethod []string `xml:"AllowedMethod"`
	AllowedHeader []string `xml:"AllowedHeader,omitempty"`
	ExposeHeader  []string `xml:"ExposeHeader,omitempty"`
	MaxAgeSeconds int      `xml:"MaxAgeSeconds,omitempty"`
}

// AccessControlPolicy is the response of GetBucketAcl / GetObjectAcl.
type AccessControlPolicy struct {
	XMLName           xml.Name `xml:"AccessControlPolicy"`
	XMLNS             string   `xml:"xmlns,attr"`
	Owner             Owner    `xml:"Owner"`
	AccessControlList struct {
		Grants []Grant `xml:"Grant"`
	} `xml:"AccessControlList"`
}

type Grant struct {
	Grantee    Grantee `xml:"Grantee"`
	Permission string  `xml:"Permission"`
}

type Grantee struct {
	XMLNSXSI    string `xml:"xmlns:xsi,attr"`
	Type        string `xml:"xsi:type,attr"`
	ID          string `xml:"ID"`
	DisplayName string `xml:"DisplayName,omitempty"`
}

// ListAllMyBucketsResult is the response of ListBuckets.
type ListAllMyBucketsResult struct {
	XMLName xml.Name `xml:"ListAllMyBucketsResult"`
	XMLNS   string   `xml:"xmlns,attr"`
	Owner   Owner    `xml:"Owner"`
	Buckets struct {
		Bucket []BucketEntry `xml:"Bucket"`
	} `xml:"Buckets"`
}

type BucketEntry struct {
	Name         string `xml:"Name"`
	CreationDate string `xml:"CreationDate"`
}

// PostResponse is the body of a browser-based POST upload response when the
// form requests success_action_status 201.
type PostResponse struct {
	XMLName  xml.Name `xml:"PostResponse"`
	XMLNS    string   `xml:"xmlns,attr,omitempty"`
	Location string   `xml:"Location"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	ETag     string   `xml:"ETag"`
}

// Write renders v as an S3 XML response body with a 200 status.
func Write(w http.ResponseWriter, v any) error {
	b, err := xml.Marshal(v)
	if err != nil {
		return fmt.Errorf("failed to marshal XML response: %w", err)
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(xml.Header))
	w.Write(b)
	return nil
}

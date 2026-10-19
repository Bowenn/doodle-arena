package events

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/credentials"
)

func TestS3ClientUsesStaticKeysOrTheDefaultChain(t *testing.T) {
	local, err := NewS3Client(t.Context(), S3Config{Endpoint: "http://seaweedfs:8333", Region: "us-east-1",
		PathStyle: true, AccessKey: "key", SecretKey: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := local.Options().Credentials.(credentials.StaticCredentialsProvider); !ok || !local.Options().UsePathStyle {
		t.Fatalf("local: credentials %T, path style %v", local.Options().Credentials, local.Options().UsePathStyle)
	}

	// No keys: the default chain (the ECS task role in AWS), virtual-hosted addressing.
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_PROFILE", "")
	aws, err := NewS3Client(t.Context(), S3Config{Region: "us-west-2"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := aws.Options().Credentials.(credentials.StaticCredentialsProvider); ok || aws.Options().Credentials == nil {
		t.Fatalf("aws: credentials %T, want the default chain", aws.Options().Credentials)
	}
	if aws.Options().UsePathStyle || aws.Options().BaseEndpoint != nil {
		t.Fatal("aws: want virtual-hosted addressing on AWS's own endpoint")
	}

	if _, err := NewS3Client(t.Context(), S3Config{Region: "us-east-1", AccessKey: "key"}); err == nil {
		t.Fatal("accepted an access key without its secret")
	}
}

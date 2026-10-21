// Package s3test starts SeaweedFS's S3 gateway for integration tests, with the same official
// image as infra/docker-compose.yml. Kafka's starter is shared: libs/go/kafkatest.
package s3test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Bowenn/gap-project_private/libs/go/tcretry"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// SeaweedFSImage matches infra/docker-compose.yml.
const SeaweedFSImage = "chrislusf/seaweedfs:4.48"

// SeaweedFS starts SeaweedFS's S3 gateway with one identity and returns its endpoint.
func SeaweedFS(t *testing.T, accessKey, secretKey string) string {
	t.Helper()
	ctx := t.Context()
	cfg := fmt.Sprintf(`{"identities":[{"name":"test","credentials":[{"accessKey":%q,"secretKey":%q}],`+
		`"actions":["Admin","Read","List","Tagging","Write"]}]}`, accessKey, secretKey)
	c, err := tcretry.Run("seaweedfs", func() (*testcontainers.DockerContainer, error) {
		return testcontainers.Run(ctx, SeaweedFSImage,
			testcontainers.WithExposedPorts("8333/tcp"),
			testcontainers.WithFiles(testcontainers.ContainerFile{
				Reader: strings.NewReader(cfg), ContainerFilePath: "/etc/s3.json", FileMode: 0o644,
			}),
			testcontainers.WithEntrypoint("weed"),
			testcontainers.WithCmd("server", "-dir=/data", "-ip.bind=0.0.0.0", "-volume.max=0",
				"-s3", "-s3.port=8333", "-s3.config=/etc/s3.json"),
			testcontainers.WithWaitStrategy(wait.ForListeningPort("8333/tcp").WithStartupTimeout(time.Minute)),
		)
	})
	testcontainers.CleanupContainer(t, c)
	if err != nil {
		t.Fatalf("start seaweedfs: %v", err)
	}
	endpoint, err := c.PortEndpoint(ctx, "8333/tcp", "http")
	if err != nil {
		t.Fatal(err)
	}
	return endpoint
}

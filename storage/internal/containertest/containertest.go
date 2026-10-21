// Package containertest starts throwaway Docker containers for backend
// integration tests. It is meant to be called from TestMain so one container
// serves a whole test package.
package containertest

import (
	"context"
	"fmt"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Images used by the backend tests. rclone's S3 server was dropped because
// concurrent PUTs into a fresh prefix sometimes stored empty objects, even
// with plain minio-go requests; versitygw does not have that problem.
const (
	RcloneImage    = "rclone/rclone:1.72"
	VersityGWImage = "versity/versitygw:v1.9.0"
)

// Server is a running container.
type Server struct {
	// Addr is host:port of the mapped port.
	Addr string
	c    testcontainers.Container
}

// Close terminates the container.
func (s *Server) Close() {
	if s == nil || s.c == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_ = s.c.Terminate(ctx)
}

// StartRclone runs `rclone <args>` and waits until port (for example "9000")
// accepts connections. An error usually means Docker is unavailable; callers
// are expected to skip their tests in that case.
func StartRclone(port string, args ...string) (*Server, error) {
	return Start(RcloneImage, nil, port, args...)
}

// StartVersityGW runs an S3 gateway backed by a POSIX directory, with the
// given root credentials, listening on port 7070.
func StartVersityGW(accessKey, secretKey string) (*Server, error) {
	return Start(VersityGWImage, map[string]string{
		"ROOT_ACCESS_KEY": accessKey,
		"ROOT_SECRET_KEY": secretKey,
	}, "7070", "--port", ":7070", "posix", "/tmp")
}

// Start runs image with env and args and waits until port accepts
// connections.
func Start(image string, env map[string]string, port string, args ...string) (*Server, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	tcp := port + "/tcp"
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        image,
			Env:          env,
			Cmd:          args,
			ExposedPorts: []string{tcp},
			WaitingFor:   wait.ForListeningPort(tcp).WithStartupTimeout(2 * time.Minute),
		},
		Started: true,
	})
	s := &Server{c: c}
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("start %s: %w", image, err)
	}
	host, err := c.Host(ctx)
	if err != nil {
		s.Close()
		return nil, err
	}
	mapped, err := c.MappedPort(ctx, tcp)
	if err != nil {
		s.Close()
		return nil, err
	}
	s.Addr = host + ":" + mapped.Port()
	return s, nil
}

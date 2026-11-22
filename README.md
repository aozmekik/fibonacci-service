# fibonacci-service

An HTTP service that returns the first `n` Fibonacci numbers as a JSON array.

```console
$ curl 'http://localhost:8080/v1/fibonacci?n=10'
[0,1,1,2,3,5,8,13,21,34]
```

## API

`GET /v1/fibonacci?n=<n>` returns the first `n` numbers of the sequence, starting from 0.
`n=0` returns `[]`, and `HEAD` returns the same headers without a body.

The URL is accepted in one form only: this path, and a query that is `n=` followed by a decimal integer without a sign or leading zeros.
It is compared as sent, so `n=05`, `n=%35`, `n=5&`, `n=5&pretty=1` and `/v1/%66ibonacci?n=5` are rejected, and `//v1/fibonacci?n=5` is not redirected.
That gives each response one URL that caches can rely on, and a parameter added later is rejected by older versions instead of being ignored.
The rules can be relaxed later, while tightening them would break clients.

Errors are JSON objects with a `code` for programs and a `message` for people, which may change:

```console
$ curl 'http://localhost:8080/v1/fibonacci?n=-1'
{"code":"invalid_argument","message":"n must not be negative"}
```

| Status | Code | When |
| --- | --- | --- |
| 400 | `invalid_argument` | the query is not in the form above, or `n` is negative or above 20000 |
| 400 | `out_of_range` | `n` is above the limit set with `--max-n`, but at most 20000 |
| 404 | `not_found` | the path is not `/v1/fibonacci` |
| 405 | `method_not_allowed` | the method is not `GET` or `HEAD` |

The two 400 codes follow the gRPC codes they are named after.
No instance of the service accepts an `invalid_argument` request, while an `out_of_range` one may work on an instance with a higher limit.
Requests that `net/http` rejects before the API sees them get a plain text 400 if malformed, or 431 with more than 12 KiB of headers, and are not counted in the metrics.

Every response of the API has an exact `Content-Length`.
Successful responses depend only on `n`, so they are sent with `Cache-Control: public, max-age=86400, immutable`.
The lifetime is a day rather than a year to limit how long a broken release can stay in caches.
Errors are sent with `Cache-Control: no-store`, since some of them depend on the configured limit.

### Large numbers

The numbers are JSON numbers, as in the task.
JSON puts no limit on their size, but most parsers do:

| `n` | Every number fits into |
| --- | --- |
| up to 79 | an IEEE 754 double, which `JSON.parse` in JavaScript and many other parsers use for all numbers |
| up to 93 | a signed 64-bit integer |
| up to 94 | an unsigned 64-bit integer |
| above 94 | arbitrary-precision integers only, such as `[]*big.Int` in Go or `int` in Python |

A parser that uses doubles silently rounds the numbers from `n=80`, and gets infinity or an error from `n=1478`.
For such clients, run the service with `--max-n=79`, so that it rejects what they cannot read correctly.
Strings would avoid the problem, but the task shows numbers, and a second format is one more thing to keep working.

## Running

```sh
go run ./cmd/fibonacci-service
```

Or as a container, from the image that CI builds from `main`:

```sh
docker run --rm --user 65534:65534 -p 8080:8080 -p 2122:2122 ghcr.io/aozmekik/fibonacci-service:latest
```

The image contains only the binary and does not set a user, so without `--user` the process runs as root.

The flags are `--listen-addr` (`:8080`), `--metrics-addr` (`:2122`, empty to disable), `--max-n` (1000, at most 20000), `--max-connections` (256), `--shutdown-timeout` (30s), `--log-level` (`info`) and `--version`, and `--help` describes them.
The API and `/healthz` are served on `--listen-addr`.
The metrics have a listener of their own on `--metrics-addr`, so they are not exposed along with the API.

On `SIGTERM` or `SIGINT` the service stops accepting connections and gives the requests in flight `--shutdown-timeout` to finish.
The default is the longest a response may take, so a response that would have completed without the shutdown still completes.
The metrics are served until the API is done, and a second signal stops the service at once.

## Deploying

```sh
kubectl apply -f deploy/fibonacci-service.yaml
kubectl rollout status deployment/fibonacci-service
kubectl port-forward service/fibonacci-service 8080:80
```

With the port forwarded, the `curl` at the top works as it is.
[The manifest](deploy/fibonacci-service.yaml) runs two replicas behind a Service, with a PodDisruptionBudget.

- The pods meet the `restricted` Pod Security Standard, which Talos warns about by default, and have a read-only root filesystem on top of it.
- There is a readiness probe and no liveness probe.
  The process has no state to get stuck in, and restarting a pod that is overloaded would only make things worse.
- A pod that is stopping keeps getting new connections until its removal from the endpoints reaches every node.
  A five-second `preStop` sleep keeps it serving meanwhile.
  The `sleep` action needs Kubernetes 1.30, since the image has no `sleep` binary.
- The memory limit comes with a `GOMEMLIMIT` just below it, see [Design decisions](#design-decisions).
- The image is a release, which CI publishes when its tag is pushed.

[hack/test/talos.sh](hack/test/talos.sh) runs the manifest as it is on a local Talos cluster in Docker, with an image built from the working tree.
After a few checks, it restarts the deployment while a pod in the cluster keeps sending requests to the service's cluster IP, each on a new connection:

```console
$ hack/test/talos.sh
...
ok: GET /v1/fibonacci?n=-1 -> {"code":"invalid_argument","message":"n must not be negative"}
ok: GET /v1/fibonacci?n=1001 -> {"code":"out_of_range","message":"n must not be greater than 1000"}
deployment.apps/fibonacci-service restarted
...
ok: 3052 requests during the rollout, none failed
```

Without the `preStop` hook, 9 to 15 requests failed in each of four rollouts, most of them refused; with it, none of 10254 did.
This was Talos 1.14.6 with Kubernetes 1.37.1 on a 7.0 kernel, as the nodes did not start on 6.8.
The Docker host needs the `br_netfilter` module, which Docker stopped loading in version 28, and the script checks for it.
The script pushes the image to a registry on `127.0.0.1`, so a buildx builder with the `docker-container` driver has to be on the host network, as it is for building Talos.

## Design decisions

### A table computed at startup

Without its closing bracket, the array of the first `n` numbers is a prefix of the array of the first `n+1`.
So the service computes the array for `--max-n` once, at startup, keeps it in one byte slice and remembers where each shorter array ends.
A request is answered by writing part of that slice and a closing bracket.
No number is computed or copied for it, and the size of the response is known before its first byte.
The handler allocates only the header values, 72 bytes in 5 allocations, as `BenchmarkFibonacci` shows.

The cost is memory and startup time, which grow with the square of `--max-n`, to about n²/10 bytes.
The times are from an Apple M1 Pro, and the memory is the peak of the process in a Linux container:

| `--max-n` | Table and largest response | Build time | Memory |
| --- | --- | --- | --- |
| 1000 | 105 kB | 0.4 ms | 5 MiB |
| 10000 | 10.5 MB | 65 ms | 28 MiB |
| 20000 | 41.8 MB | 310 ms | 89 MiB |

That is why `--max-n` has a ceiling of 20000 and a default of 1000, where the largest response is 105 kB, while at 10000 a request of a hundred bytes gets 10 MB back.
Building the table leaves about three times its size in garbage, and the Go runtime collects only once the heap has doubled, so the process holds about twice the table.
With the `GOMEMLIMIT` of 56 MiB that the manifest sets, it collects sooner and takes 51 MiB at `--max-n=20000`, without starting any slower.

Computing and streaming the numbers for each request would need no table, but:

- A request would cost the build time above in CPU, 65 ms for `n=10000`, so the service would need a limit on concurrent requests and a policy for the ones over it.
- A response cut short by a shutdown or a deadline would still end properly in chunked encoding.
  The client would get a part of the array with a 200, unless the connection were aborted on purpose.
- A limit set too high would show up under load, in the middle of responses, instead of at startup on the first pod of a rollout.

### Limits on what a client can hold

The server gives a client 10 seconds to send a request, 30 seconds to read the response and 60 seconds to send the next one, and it reads at most 12 KiB of headers.
That bounds how long a connection is held, but not how many there are.
In a Linux container at the default `--max-n`, an idle connection took 25 KiB, and one whose client did not read its response took 125 KiB, mostly in the kernel's send buffer, which `GOMEMLIMIT` does not cover.
3000 idle connections from a single client were enough to get the 64 MiB container killed.
So each of the two servers keeps at most `--max-connections` open, 256 by default, and closes the ones over it right away, which the metrics count.
With that, 1000 clients that did not read kept the container under 37 MiB.
The limit counts every open connection, also the ones that well-behaved clients keep open between requests.
On the Talos cluster, with 256 such connections open to one pod, every new connection to it was reset, the ones of the readiness probe included.
The pod left the endpoints 13 seconds later, after three failed probes, and came back once the server had closed the idle connections after 60 seconds.
Until it was out, half of the new connections to the service failed, so `--max-connections` has to stay above what the clients keep open.
At `--max-n=20000` a connection whose client does not read takes up to 0.75 MiB, so a larger `--max-n` needs a lower `--max-connections` or more memory.

A response has to be read within the write timeout of 30 seconds, which is not a flag.
That is no problem at the default limit, but at `--max-n=20000` it takes a client reading 11 Mbit/s.
A slower client gets part of the array, which it can tell by the `Content-Length`, and the request counts as a 200 and as an aborted response.

### What is left out

TLS, rate limits and limits per client need certificates, the client's identity and state shared between the replicas, so they belong to the proxy in front of the service.
So does compression, which would bring back the work per request that the table removes.
There is no `ETag`, so once a cached response is a day old, a cache fetches it again instead of revalidating it.
There are no range requests and no access log, as the metrics count the requests and only the proxy knows the client.

## Metrics

Next to the standard Go and process metrics, `/metrics` on `--metrics-addr` has:

| Name | Type | Notes |
| --- | --- | --- |
| `fibonacci_service_http_requests_total` | counter | by status `code` |
| `fibonacci_service_http_requests_in_flight` | gauge | |
| `fibonacci_service_http_request_duration_seconds` | histogram | mostly the time the client takes to read the response |
| `fibonacci_service_http_response_size_bytes` | histogram | of the body, so it shows how close the requests get to `--max-n` |
| `fibonacci_service_http_responses_aborted_total` | counter | responses the server could not finish writing, as the client left or read too slowly |
| `fibonacci_service_http_connections_rejected_total` | counter | connections closed right away, as `--max-connections` were open, on either server |
| `fibonacci_service_build_info` | gauge | `version` and `revision` labels |

There is no label for the path or for `n`, as the client picks both, and requests to `/healthz` are not counted.

A response counts as aborted only if the client leaves while the server is still writing it.
Once the rest of a response is in the connection's buffers, the server cannot tell whether it is read.
In a Linux container, 3 of 5 responses of 105 kB that the client left after the headers were counted, and all of them from 420 kB, so the counter is a lower bound that matters most for a large `--max-n`.
A handler that panics is not counted at all: `net/http` logs the stack trace and closes the connection without a response.

## Maintaining it for five years

Under `/v1`, the path, the accepted form of `n`, the response format, the error codes and the metric names stay as they are.
Error messages and log lines may change.
A breaking change would be `/v2`, served next to `/v1` until the clients have moved, which needs a label for the version on the metrics first.
A new parameter does not break anything, as older versions reject it instead of ignoring it.
If clients ever need more numbers than one response should carry, `offset` and `limit` can page through the table, which can serve any slice of itself without copying.

The alerts worth having from the first day, from these metrics and kube-state-metrics:

- Fewer ready replicas than desired for more than a few minutes.
- Any rate of rejected connections, which is either abuse or a `--max-connections` that is too low.
- A growing share of aborted responses after `--max-n` is raised.
- A restart as `OOMKilled`, which means that the memory limit, `GOMEMLIMIT`, `--max-n` and `--max-connections` no longer fit together.
- A growing share of 400s, which can mean that the clients want more than `--max-n`.
  Telling `out_of_range` apart from malformed requests needs the error code as a label, which the metrics do not have yet.

Releases are tags, with images for amd64 and arm64.
Neither the tags nor the images are signed yet, and the images come without an SBOM, which the kres build turns off.
So the tags should be signed, and the release workflow should sign the images with cosign and attach an SBOM.
A release has no changelog either, which `hack/release.sh changelog` can write from the commit messages.
Renovate keeps the Go modules current once its app is installed for the repository, and the `govulncheck` in `make lint` catches known vulnerabilities that the service can reach.
It leaves the workflows alone, as kres regenerates the ones it writes, so the actions and the conform version pinned in `build.yaml` have to be bumped by hand.
`make rekres` brings newer Go and linters along with kres.
`hack/test/talos.sh` should run in CI on every pull request and against each new Talos and Kubernetes release, on a runner with `br_netfilter`.

## Development

Everything is built, tested and linted in containers, so Docker with buildx and GNU make 4 are all it takes, which is `gmake` after `brew install make` on macOS.
`make help` lists the targets, and the tests also run without containers, along with a fuzz test that works out the expected status and error code of any request target on its own:

```sh
make unit-tests unit-tests-race lint image-fibonacci-service
go test -race ./...
go test -fuzz FuzzFibonacci ./internal/api
```

`make image-fibonacci-service IMAGE_TAG=dev CI_ARGS=--load` also loads the image into the local Docker as `ghcr.io/aozmekik/fibonacci-service:dev`, with `PLATFORM=linux/arm64` added on an arm64 machine.
A buildx builder with the `docker-container` driver can load it only where Docker uses the containerd image store, as the Makefile builds a manifest list.

[kres](https://github.com/siderolabs/kres) generates the Makefile, the Dockerfile and the linter configuration from [.kres.yaml](.kres.yaml), and `make rekres` updates them.
Run it without `GITHUB_TOKEN` set, as with a token kres also changes the repository's branch protection and invites its bot.
It also writes four GitHub workflows that run only on Sidero Labs' infrastructure, so remove them again with `git clean -f .github/workflows`.
[.github/workflows/build.yaml](.github/workflows/build.yaml) runs the same targets on GitHub-hosted runners, and checks every commit of a pull request against [.conform.yaml](.conform.yaml).
On macOS the Makefile prints a harmless `sed` error on every run, unless GNU sed comes first in `PATH`.

### Releasing

A release is a tag, and CI publishes its image.
The version is also in [internal/version/data/tag](internal/version/data/tag) and in the manifest, and a commit before the tag sets both.
It goes through a pull request like any other, and CI takes the version from its message, as the tag does not exist yet:

```sh
make generate ABBREV_TAG=v0.2.0
# set the tag of the image in deploy/fibonacci-service.yaml to v0.2.0
git add internal/version/data/tag deploy/fibonacci-service.yaml
hack/release.sh commit v0.2.0
# once the pull request is merged
git switch main
git pull
git tag v0.2.0
git push origin v0.2.0
```

## License

Mozilla Public License 2.0, see [LICENSE](LICENSE).

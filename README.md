# aludel-cloudscale

> An aludel is the sealed vessel an alchemist sublimes into — a container you put things in and close.

`aludel-cloudscale` is a [COSI](https://container-object-storage-interface.sigs.k8s.io/) driver for [cloudscale.ch](https://www.cloudscale.ch) object storage. It lets a Kubernetes workload ask for an S3 bucket with a `BucketClaim` and receive credentials in a Secret, without anybody touching the cloudscale.ch control panel.

Documentation: https://docs.helmetica.io/aludel-cloudscale/

## Development

```bash
just build
just test
just docs   # preview the docs
```

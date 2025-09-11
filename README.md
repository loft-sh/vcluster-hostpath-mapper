## Usage and installation

Vcluster internal logging relies on separate component called the [Hostpath Mapper](https://github.com/loft-sh/vcluster-hostpath-mapper). This will make sure to resolve the correct virtual pod and container names to their physical counterparts.
To deploy this component, its basically a 2 step process

### Update the vcluster

You would want to create the vcluster with the following `values.yaml`:
```
controlPlane:
  hostPathMapper:
    enabled: true
```

* For new vcluster run `vcluster create <vcluster_name> -f values.yaml`
* For existing vcluster run `vcluster create --upgrade <vcluster_name> -f values.yaml`

### Deploy the Hostpath Mapper Daemonset

Now that the vcluster itself is ready, we can deploy the hostpath mapper component. We need the following 2 pieces of information for this:
* The Hostpath Mapper has to be deployed in the same namespace and the target vcluster
* We need to set the `.Values.VclusterReleaseName` value when deploying this helm chart equal to the name of the target vcluster

To sum up, if your vcluster is named `my-vcluster` and is deployed in namespace `my-namespace` then you should run
```shell
helm install vcluster-hpm vcluster-hpm \
    --repo https://charts.loft.sh \ 
    -n my-namespace \
    --set VclusterReleaseName=my-vcluster
```

Once deployed successfully a new Daemonset component of vcluster would start running on every node used by the vcluster workloads.

We can now install our desired logging stack and start collecting the logs.

## Versioning

| vcluster        | hostpath-mapper |
|-----------------|-----------------|
| v0.21 and above | 0.2.x and above |
| v0.20 and below | 0.1.x           |

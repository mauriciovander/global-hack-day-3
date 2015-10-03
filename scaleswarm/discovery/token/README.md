#discovery.hub.docker.com

Docker Swarm comes with a simple discovery service built into the [Docker Hub](http://hub.docker.com)

#####Create a new cluster
`-> POST https://discovery.hub.docker.com/v1/clusters`

`<- <token>`

#####Add new nodes to a cluster
`-> POST https://discovery.hub.docker.com/v1/clusters/<token>?ttl=<ttl> Request body: "<ip>:<port1>"`

`<- OK`

`-> POST https://discovery.hub.docker.com/v1/clusters/<token>?ttl=<ttl> Request body: "<ip>:<port2>")`

`<- OK`


#####List nodes in a cluster
`-> GET https://discovery.hub.docker.com/v1/clusters/<token>`

`<- ["<ip>:<port1>", "<ip>:<port2>"]`


#####Delete a cluster (all the nodes in a cluster)
`-> DELETE https://discovery.hub.docker.com/v1/clusters/<token>`

`<- OK`
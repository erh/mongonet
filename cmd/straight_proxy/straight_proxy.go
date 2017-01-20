package main

import "flag"

import "github.com/erh/mongonet"

func main() {

	bindHost := flag.String("host", "127.0.0.1", "what to bind to")
	bindPort := flag.Int("port", 9999, "what to bind to")
	mongoHost := flag.String("mongoHost", "127.0.0.1", "host mongo is on")
	mongoPort := flag.Int("mongoPort", 27017, "port mongo is on")

	flag.Parse()

	pc := mongonet.NewProxyConfig(*bindHost, *bindPort, *mongoHost, *mongoPort)
	pc.MongoSSLSkipVerify = true

	proxy := mongonet.NewProxy(pc)

	err := proxy.Run()
	if err != nil {
		panic(err)
	}
}

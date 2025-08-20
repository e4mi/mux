A simple web server for managing multiple apps.

Usage: mux -dir <dir> [options]

Proxies requests to http://*.localhost to applications in <dir>/*.
Autostarts apps with $PORT set to random port.
App command is configured in Procfile in format: web: <cmd>

  -dir string
        Directory to serve applications from
  -disable
        Disable and uninstall service
  -enable
        Enable and install service
  -host string
        Domain to serve applications on (default "localhost")
  -port string
        Port to listen on (default "80")
  -user string
        Run service as user (default "emily")

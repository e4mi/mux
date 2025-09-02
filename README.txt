
Autostarts apps and serves them at subdomains. Reloads them on changes.

Setup with:
  mux -enable

Setup apps:
  ~/Web/APP/Procfile:  web: ./start.sh $PORT
  ~/Web/APP/.watch:    src/*

Visiting http://APP.localhost will start and serve the app.

Options:
  -dir string
    	directory to serve applications from (default "~/Web")
  -disable
    	disable start on boot
  -enable
    	start on boot
  -host string
    	serve on http://*.HOST (default "localhost")
  -port string
    	port to listen on (default "7777")
  -verbose
    	verbose logging


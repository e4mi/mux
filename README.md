# File based web server multiplexer

- run `./mux` server
- create `~/apps/myapp/Procfile` with `web: ./myserver`
- visit `http://myapp.localhost`, it will run `PORT=$RANDOM ./myserver` and proxy to it
- use `~/apps/www` to serve http://localhost

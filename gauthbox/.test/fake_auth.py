#! /usr/bin/env nix-shell
#! nix-shell -i python3 -p "python3.withPackages (p: with p; [ fastapi uvicorn ])"

from fastapi import FastAPI, Response, Request
from fastapi.responses import FileResponse

app = FastAPI()

@app.post('/auth')
async def auth(request: Request):
    print(request)
    return {}


if __name__ == "__main__":
    import uvicorn
    uvicorn.run('fake_auth:app', host="0.0.0.0", port=8001, reload=True)

#! /usr/bin/env nix-shell
#! nix-shell -i python3 -p "python3.withPackages (p: with p; [ fastapi uvicorn ])"

from fastapi import FastAPI, Response, Request

app = FastAPI()

@app.get('/config/{name}')
async def config(name: str):
    return {
        "badge_reader": {
            "name": "HID OMNIKEY 5427 CK",
            "timeout_ms": 200
        },
        "badge_auth": {
            "url_template": "http://control.shop:8000/auth?tool=%s&badge={{.badge}}&action={{.state}}&minutes={{.duration}}" % name,
            "usage_duration_minutes": 10
        },
        "relay": {
            "pin": 23,
            "debounce_ms": 25
        },
        "current_sensing": {
            "pin": 24,
            "debounce_ms": 200
        },
        "green_led": {
            "pin": 5
        },
        "red_led": {
            "pin": 6
        },
        "mqtt": {
            "broker": "mqtt://control.shop:1883",
            "topic": "shop"
        },
        "idle_duration_s": 5
    }


@app.post('/auth')
async def auth(request: Request):
    print(request)
    return {}


if __name__ == "__main__":
    import uvicorn
    uvicorn.run('fake_control:app', host="0.0.0.0", port=8000, reload=True)

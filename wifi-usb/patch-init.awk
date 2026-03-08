/^# Setup network interfaces$/ {
    print
    getline
    print
    print "\tif [ -f /etc/wpa_supplicant/wpa_supplicant.conf ]; then"
    print "\t\tmkdir -p /var/run/wpa_supplicant"
    print "\t\twhile ! ip link show wlan0 >/dev/null 2>&1; do sleep 0.1; done"
    print "\t\twpa_supplicant -B -i wlan0 -C /var/run/wpa_supplicant -c /etc/wpa_supplicant/wpa_supplicant.conf"
    print "\t\tsleep 0.5"
    print "\t\twhile true; do"
    print "\t\t\tstatus=$(wpa_cli -p /var/run/wpa_supplicant status 2>&1)"
    print "\t\t\techo \"$status\" | grep -q wpa_state=COMPLETED && break"
    print "\t\t\tsleep 0.5"
    print "\t\tdone"
    print "\tfi"
    next
}
{ print }
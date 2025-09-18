# Raspberry Pi Homelab Setup: Nginx Proxy Manager + fail2ban

A complete, production-ready setup for Raspberry Pi running Nginx Proxy Manager (NPM) with fail2ban security, automatic SSL certificates, and container management.

## 🏗️ Architecture

- **Nginx Proxy Manager**: Reverse proxy with web UI and automatic SSL certificates
- **fail2ban**: Intrusion prevention system for SSH and web services
- **Watchtower**: Automatic container updates
- **UFW**: Uncomplicated Firewall for network security

## 📋 Prerequisites

- Raspberry Pi with Docker installed
- Domain name pointing to your public IP
- Router port forwarding: 80, 443 → Pi IP
- SSH access to Pi

## 🚀 Quick Setup

### Step 1: Initial System Setup

```bash
# SSH into your Pi
ssh pi@192.168.1.200

# Update system
sudo apt update && sudo apt upgrade -y

# Install essential packages
sudo apt install -y curl wget git vim htop tree unzip fail2ban ufw

# Configure firewall
sudo ufw reset
sudo ufw default deny incoming
sudo ufw default allow outgoing

# Allow required ports
sudo ufw allow 22/tcp comment 'SSH'
sudo ufw allow 80/tcp comment 'HTTP NPM'
sudo ufw allow 443/tcp comment 'HTTPS NPM'
sudo ufw allow 8181/tcp comment 'NPM Web Interface'

# Enable firewall
sudo ufw --force enable

# Verify Docker is running
sudo systemctl enable docker
sudo systemctl start docker
docker --version
```

### Step 2: Configure fail2ban

```bash
# Create fail2ban configuration
sudo tee /etc/fail2ban/jail.local << 'EOF'
[DEFAULT]
bantime = 3600
findtime = 600
maxretry = 3

[sshd]
enabled = true
port = ssh
filter = sshd
logpath = /var/log/auth.log
maxretry = 3
EOF

# Test and start fail2ban
sudo fail2ban-client -t
sudo systemctl enable fail2ban
sudo systemctl start fail2ban

# Verify fail2ban status
sudo fail2ban-client status
```

### Step 3: Create Project Structure

```bash
# Create homelab directory structure
mkdir -p ~/homelab/{data,scripts,backups,logs}
mkdir -p ~/homelab/data/nginx/letsencrypt

# Set permissions
chmod -R 755 ~/homelab
cd ~/homelab
```

### Step 4: Environment Configuration

```bash
# Create environment file
cat > ~/homelab/.env << 'EOF'
# Domain Configuration
DOMAIN_NAME=nixndme.com
PUBLIC_IP=49.204.124.106
PI_LOCAL_IP=192.168.1.200

# Timezone
TZ=Asia/Kolkata

# Docker Network
DOCKER_SUBNET=172.20.0.0/16
EOF

# Secure the file
chmod 600 ~/homelab/.env
```

### Step 5: Docker Compose Configuration

```bash
# Create docker-compose.yml
cat > ~/homelab/docker-compose.yml << 'EOF'
version: '3.8'

services:
  # Nginx Proxy Manager
  nginx-proxy-manager:
    image: 'jc21/nginx-proxy-manager:latest'
    container_name: nginx-proxy-manager
    restart: unless-stopped
    ports:
      - '80:80'      # HTTP
      - '443:443'    # HTTPS
      - '8181:81'    # Admin Web UI
    environment:
      DB_SQLITE_FILE: "/data/database.sqlite"
      DISABLE_IPV6: 'true'
    volumes:
      - ./data/nginx:/data
      - ./data/nginx/letsencrypt:/etc/letsencrypt
      - /etc/localtime:/etc/localtime:ro
    healthcheck:
      test: ["CMD", "curl", "-f", "http://localhost:81/api/"]
      interval: 30s
      timeout: 10s
      retries: 5
      start_period: 60s
    networks:
      homelab:
        ipv4_address: 172.20.0.10

  # Watchtower - Automatic Updates
  watchtower:
    image: containrrr/watchtower:latest
    container_name: watchtower
    restart: unless-stopped
    environment:
      TZ: '${TZ}'
      WATCHTOWER_CLEANUP: 'true'
      WATCHTOWER_SCHEDULE: '0 0 4 * * 0'  # Weekly Sunday 4 AM
      WATCHTOWER_INCLUDE_RESTARTING: 'true'
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - /etc/localtime:/etc/localtime:ro
    networks:
      - homelab

networks:
  homelab:
    driver: bridge
    ipam:
      driver: default
      config:
        - subnet: ${DOCKER_SUBNET}
          gateway: 172.20.0.1

volumes:
  nginx-data:
    driver: local
EOF
```

### Step 6: Management Scripts

```bash
# Create management script
cat > ~/homelab/scripts/manage.sh << 'EOF'
#!/bin/bash
# Homelab Management Script

cd ~/homelab

case "$1" in
    start)
        echo "Starting homelab services..."
        docker-compose up -d
        ;;
    stop)
        echo "Stopping homelab services..."
        docker-compose down
        ;;
    restart)
        echo "Restarting homelab services..."
        docker-compose restart
        ;;
    logs)
        docker-compose logs -f --tail=50
        ;;
    status)
        docker-compose ps
        ;;
    update)
        echo "Updating containers..."
        docker-compose pull
        docker-compose up -d
        docker image prune -f
        ;;
    backup)
        echo "Creating backup..."
        mkdir -p backups
        DATE=$(date +%Y%m%d_%H%M%S)
        tar --exclude='./backups' --exclude='./logs/*.log' -czf "backups/homelab-backup-$DATE.tar.gz" .
        echo "Backup created: backups/homelab-backup-$DATE.tar.gz"
        ;;
    fail2ban-status)
        echo "fail2ban Status:"
        sudo fail2ban-client status
        ;;
    fail2ban-unban)
        if [ -z "$2" ]; then
            echo "Usage: $0 fail2ban-unban <IP_ADDRESS>"
            exit 1
        fi
        echo "Unbanning IP: $2"
        sudo fail2ban-client unban "$2"
        ;;
    *)
        echo "Usage: $0 {start|stop|restart|logs|status|update|backup|fail2ban-status|fail2ban-unban}"
        echo ""
        echo "Available commands:"
        echo "  start            - Start all services"
        echo "  stop             - Stop all services" 
        echo "  restart          - Restart all services"
        echo "  logs             - View service logs"
        echo "  status           - Show service status"
        echo "  update           - Update all containers"
        echo "  backup           - Create backup"
        echo "  fail2ban-status  - Show fail2ban status"
        echo "  fail2ban-unban   - Unban an IP address"
        ;;
esac
EOF

chmod +x ~/homelab/scripts/manage.sh

# Create health check script
cat > ~/homelab/scripts/health-check.sh << 'EOF'
#!/bin/bash
# Homelab Health Check Script

echo "=== Homelab Health Check ==="
echo "Date: $(date)"
echo ""

# Check Docker
echo "🐳 Docker Status:"
if systemctl is-active --quiet docker; then
    echo "✅ Docker service is running"
else
    echo "❌ Docker service is not running"
fi

# Check containers
echo ""
echo "📦 Container Status:"
cd ~/homelab
docker-compose ps

# Check NPM accessibility
echo ""
echo "🌐 Service Accessibility:"
if curl -s -o /dev/null -w "%{http_code}" http://localhost:8181 | grep -q "200\|302"; then
    echo "✅ NPM Web Interface accessible"
else
    echo "❌ NPM Web Interface not accessible"
fi

# Check ports
echo ""
echo "🔌 Port Status:"
for port in 80 443 8181; do
    if netstat -tuln | grep -q ":$port "; then
        echo "✅ Port $port is open"
    else
        echo "❌ Port $port is not open"
    fi
done

# Check fail2ban
echo ""
echo "🛡️  fail2ban Status:"
if systemctl is-active --quiet fail2ban; then
    echo "✅ fail2ban is running"
    echo "📊 Current bans:"
    sudo fail2ban-client status sshd 2>/dev/null | grep "Banned IP list" || echo "No active bans"
else
    echo "❌ fail2ban is not running"
fi

# Check disk space
echo ""
echo "💾 Disk Usage:"
df -h / | tail -1 | awk '{print "Used: " $3 "/" $2 " (" $5 ")"}'

# Check memory
echo ""
echo "🧠 Memory Usage:"
free -h | awk 'NR==2{printf "Used: %s/%s (%.2f%%)\n", $3,$2,$3*100/$2 }'

echo ""
echo "=== Health Check Complete ==="
EOF

chmod +x ~/homelab/scripts/health-check.sh
```

### Step 7: Auto-Startup Configuration

```bash
# Create systemd service for auto-start
sudo tee /etc/systemd/system/homelab.service << EOF
[Unit]
Description=Homelab Docker Services
Requires=docker.service
After=docker.service network-online.target
Wants=network-online.target

[Service]
Type=oneshot
RemainAfterExit=yes
User=$(whoami)
Group=docker
WorkingDirectory=$HOME/homelab
ExecStart=/usr/bin/docker-compose up -d
ExecStop=/usr/bin/docker-compose down
TimeoutStartSec=300

[Install]
WantedBy=multi-user.target
EOF

# Enable auto-start
sudo systemctl daemon-reload
sudo systemctl enable homelab.service
sudo systemctl start homelab.service
```

### Step 8: Deploy Services

```bash
cd ~/homelab

# Set proper permissions
sudo chown -R $USER:$USER ~/homelab/data

# Start services
docker-compose up -d

# Wait for services to initialize
sleep 60

# Check status
docker-compose ps
```

## 🔐 NPM Configuration

### Initial Setup

1. **Access NPM**: Open `http://192.168.1.200:8181` in browser
2. **Login**: Use `admin@example.com` / `changeme`
3. **Change Password**: Go to Users → Edit admin user → Update credentials

### Configure Proxy Host (Example: Proxmox)

1. **Create Proxy Host**:
   - Domain Names: `master01.nixndme.com`
   - Scheme: `https`
   - Forward Hostname/IP: `192.168.1.50`
   - Forward Port: `8006`
   - Check: Cache Assets, Block Common Exploits, Websockets Support

2. **SSL Tab**:
   - SSL Certificate: "Request a new SSL Certificate"
   - Check: Force SSL, HTTP/2 Support, HSTS Enabled
   - Email: your-email@example.com
   - Check: I Agree to Let's Encrypt TOS

3. **Advanced Tab** (for Proxmox):
```nginx
# Proxmox VE specific configuration
proxy_set_header Host $host;
proxy_set_header X-Real-IP $remote_addr;
proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
proxy_set_header X-Forwarded-Proto $scheme;

# Handle self-signed certificates
proxy_ssl_verify off;

# WebSocket support for console
proxy_http_version 1.1;
proxy_set_header Upgrade $http_upgrade;
proxy_set_header Connection "upgrade";

# Timeouts
proxy_connect_timeout 60s;
proxy_send_timeout 60s;
proxy_read_timeout 60s;

# Buffer settings
proxy_buffering off;
client_max_body_size 128m;
```

## 📊 Monitoring & Management

### Daily Operations

```bash
# Check service status
~/homelab/scripts/manage.sh status

# View logs
~/homelab/scripts/manage.sh logs

# Health check
~/homelab/scripts/health-check.sh

# Check fail2ban status
~/homelab/scripts/manage.sh fail2ban-status
```

### Maintenance

```bash
# Update containers
~/homelab/scripts/manage.sh update

# Create backup
~/homelab/scripts/manage.sh backup

# Restart services
~/homelab/scripts/manage.sh restart
```

### Security Management

```bash
# Check fail2ban status
sudo fail2ban-client status

# Check SSH jail
sudo fail2ban-client status sshd

# Unban an IP
sudo fail2ban-client unban 192.168.1.100

# Check firewall status
sudo ufw status
```

## 🔗 Service URLs

- **NPM Admin**: `http://192.168.1.200:8181`
- **External Services**: `https://subdomain.yourdomain.com` (after proxy configuration)

## 🛡️ Security Features

- **fail2ban**: Automatic IP banning for SSH attacks
- **UFW**: Firewall with minimal required ports
- **Auto SSL**: Let's Encrypt certificates via NPM
- **Security Headers**: Implemented via NPM proxy hosts
- **Auto Updates**: Weekly container updates via Watchtower

## 🔧 Troubleshooting

### Common Issues

1. **NPM not accessible**:
   ```bash
   docker logs nginx-proxy-manager
   curl -I http://localhost:8181
   ```

2. **fail2ban not working**:
   ```bash
   sudo systemctl status fail2ban
   sudo fail2ban-client -t
   ```

3. **SSL certificate issues**:
   - Ensure domain points to public IP
   - Check port forwarding (80, 443)
   - Verify DNS propagation

### Log Locations

- **NPM Logs**: `docker logs nginx-proxy-manager`
- **fail2ban Logs**: `/var/log/fail2ban.log`
- **Auth Logs**: `/var/log/auth.log`
- **UFW Logs**: `/var/log/ufw.log`

## 📁 File Structure

```
~/homelab/
├── .env                    # Environment variables
├── docker-compose.yml     # Container definitions
├── data/
│   └── nginx/             # NPM data and configs
│       ├── letsencrypt/   # SSL certificates
│       └── database.sqlite # NPM database
├── scripts/
│   ├── manage.sh          # Management script
│   └── health-check.sh    # Health monitoring
├── backups/               # Backup files
└── logs/                  # Application logs
```

## 🚀 Next Steps

1. Configure DNS records for your domain
2. Set up router port forwarding
3. Create proxy hosts in NPM for your services
4. Set up regular backups
5. Monitor logs and security alerts

---

**Note**: Replace `192.168.1.200`, `nixndme.com`, and service IPs with your actual values.
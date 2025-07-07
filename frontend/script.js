document.addEventListener('DOMContentLoaded', () => {
    // --- DOM Element References ---
    const statusMessage = document.getElementById('statusMessage');
    const devicesTabBtn = document.getElementById('devicesTabBtn');
    const logsTabBtn = document.getElementById('logsTabBtn');
    const configTabBtn = document.getElementById('configTabBtn');
    const devicesPage = document.getElementById('devicesPage');
    const logsPage = document.getElementById('logsPage');
    const configPage = document.getElementById('configPage');
    const configureButton = document.getElementById('configureButton');
    const startButton = document.getElementById('startButton');
    const stopButton = document.getElementById('stopButton');
    const acDomainInput = document.getElementById('acDomainInput');
    const jsonNotifyEndpointInput = document.getElementById('jsonNotifyEndpointInput');
    const devicePrefixesInput = document.getElementById('devicePrefixes');
    const inactivityRulesInput = document.getElementById('inactivityRules');
    const connectedList = document.getElementById('connectedList');
    const blacklistedList = document.getElementById('blacklistedList');
    const apList = document.getElementById('apList');
    const logList = document.getElementById('logList');
    const loginModal = document.getElementById('loginModal');
    const loginButton = document.getElementById('loginButton');
    const cancelLoginButton = document.getElementById('cancelLoginButton');
    const passwordInput = document.getElementById('password');
    const loginError = document.getElementById('loginError');

    let isAdminLoggedIn = false;

    // --- Tab Navigation & Login Modal Logic ---
    devicesTabBtn.addEventListener('click', () => showPage('devices'));
    logsTabBtn.addEventListener('click', () => showPage('logs'));
    configTabBtn.addEventListener('click', () => { //
        if (!isAdminLoggedIn) { //
            loginModal.style.display = 'flex'; //
        } else {
            showPage('config'); //
        }
    });

    function showPage(pageName) {
        devicesPage.style.display = 'none'; //
        logsPage.style.display = 'none'; //
        configPage.style.display = 'none'; //

        devicesTabBtn.classList.remove('active'); //
        logsTabBtn.classList.remove('active'); //
        configTabBtn.classList.remove('active'); //

        if (pageName === 'devices') {
            devicesPage.style.display = 'block'; //
            devicesTabBtn.classList.add('active'); //
        } else if (pageName === 'logs') {
            logsPage.style.display = 'block'; //
            logsTabBtn.classList.add('active'); //
        } else if (pageName === 'config') {
            configPage.style.display = 'block'; //
            configTabBtn.classList.add('active'); //
            fetchAndDisplayConfig(); //
        }
    }

    loginButton.addEventListener('click', () => {
        // This is a simple, client-side password check.
        if (passwordInput.value === 'Life@2024') { //
            isAdminLoggedIn = true; //
            loginModal.style.display = 'none'; //
            loginError.textContent = ''; //
            passwordInput.value = ''; //
            showPage('config'); //
        } else {
            loginError.textContent = 'Invalid password.'; //
        }
    });

    cancelLoginButton.addEventListener('click', () => {
        loginModal.style.display = 'none'; //
        passwordInput.value = ''; //
        loginError.textContent = ''; //
        showPage('devices'); //
    });

    // --- Configuration Functions ---
    configureButton.addEventListener('click', async () => {
        const payload = {
            acDomain: acDomainInput.value, //
            jsonEndpoint: jsonNotifyEndpointInput.value, //
            devicePrefixes: devicePrefixesInput.value, //
            inactivityRules: inactivityRulesInput.value, //
        };
        try {
            const response = await fetch('/api/config', { //
                method: 'POST', //
                headers: { 'Content-Type': 'application/json' }, //
                body: JSON.stringify(payload) //
            });
            const result = await response.json(); //
            statusMessage.textContent = result.message || 'Configuration updated.'; //
            statusMessage.style.color = response.ok ? 'green' : 'red'; //
            if (response.ok) fetchAndDisplayConfig(); //
        } catch (error) {
            statusMessage.textContent = 'Network error or server unreachable.'; //
            statusMessage.style.color = 'red'; //
        }
    });

    startButton.addEventListener('click', async () => {
        try {
            const response = await fetch('/api/start-services', { method: 'POST' }); //
            const result = await response.json();
            statusMessage.textContent = result.message; //
            statusMessage.style.color = response.ok ? 'green' : 'red'; //
        } catch (error) {
            statusMessage.textContent = 'Network error or server unreachable.'; //
            statusMessage.style.color = 'red'; //
        }
    });

    stopButton.addEventListener('click', async () => {
        try {
            const response = await fetch('/api/stop-services', { method: 'POST' }); //
            const result = await response.json();
            statusMessage.textContent = result.message; //
            statusMessage.style.color = response.ok ? 'green' : 'red'; //
        } catch (error) {
            statusMessage.textContent = 'Network error or server unreachable.'; //
            statusMessage.style.color = 'red'; //
        }
    });

    async function fetchAndDisplayConfig() {
        try {
            const response = await fetch('/api/get-config'); //
            if (!response.ok) throw new Error(`HTTP error! status: ${response.status}`); //
            const config = await response.json(); //
            document.getElementById('currentACDomain').textContent = config.acDomain || 'N/A'; //
            acDomainInput.value = config.acDomain || ''; //
            jsonNotifyEndpointInput.value = config.jsonNotifyEndpoint || ''; //
        } catch (error) {
            document.getElementById('currentACDomain').textContent = 'Error loading config.'; //
        }
    }

    // --- Data Fetching and UI Updating Functions ---

    // Generic fetch function to handle errors and inactive services
    async function fetchData(url) {
        try {
            const response = await fetch(url);
            if (response.status === 503) { // 503 Service Unavailable
                return { error: 'services-down', data: [] };
            }
            if (!response.ok) {
                console.error(`Fetch error for ${url}:`, response.statusText);
                return { error: 'fetch-failed', data: [] };
            }
            return { error: null, data: await response.json() };
        } catch (e) {
            console.error(`Network error for ${url}:`, e);
            return { error: 'network-error', data: [] };
        }
    }

    // Fetch and update all lists that are currently visible
    async function fetchAllLists() {
        if (devicesPage.style.display !== 'none') {
            const { data: connected } = await fetchData('/api/connected-devices');
            updateConnectedList(connected);
            const { data: blacklisted } = await fetchData('/api/blacklisted-devices');
            updateBlacklistedList(blacklisted);
        }
        if (configPage.style.display !== 'none') {
            const { data: aps } = await fetchData('/api/get-aps');
            updateAPList(aps);
        }
        if (logsPage.style.display !== 'none') {
            const { data: logs } = await fetchData('/api/connection-logs');
            updateConnectionLogsList(logs);
        }
    }

    function updateAPList(aps) {
        apList.innerHTML = ''; //
        if (aps && aps.length > 0) {
            aps.forEach(ap => {
                const li = document.createElement('li');
                // Use a different structure for better layout control
                li.innerHTML = `<div class="device-info"><strong>${ap.mac}</strong><small>Status: ${ap.status} | IP: ${ap.localip || 'N/A'}</small></div>`;
                apList.appendChild(li);
            });
        } else {
            apList.innerHTML = '<li>No active APs found.</li>'; //
        }
    }

    function updateConnectedList(devices) {
        connectedList.innerHTML = ''; //
        if (devices && devices.length > 0) {
            devices.forEach(device => {
                const li = document.createElement('li');
                li.innerHTML = `
                    <div class="device-info">
                        <strong>${device.name} (${device.mac})</strong>
                        <small>AP: ${device.ap} | Type: ${device.deviceType} | Status: ${device.connectionState}</small>
                    </div>
                    <div class="action-buttons">
                        <button class="notify-btn" data-mac="${device.mac}" data-name="${device.name}">Notify</button>
                        <button class="disconnect-btn" data-mac="${device.mac}">Disconnect</button>
                        <button class="blacklist-btn" data-mac="${device.mac}" data-name="${device.name}">Blacklist</button>
                    </div>`;
                connectedList.appendChild(li);
            });
        } else {
            connectedList.innerHTML = '<li>No connected devices.</li>'; //
        }
        addDeviceActionListeners(connectedList);
    }

    function updateBlacklistedList(devices) {
        blacklistedList.innerHTML = ''; //
        if (devices && devices.length > 0) {
            devices.forEach(device => {
                const li = document.createElement('li');
                // Display the stored name, or the MAC as a fallback
                li.innerHTML = `
                    <div class="device-info">
                        <strong>${device.name || 'N/A'} (${device.mac})</strong>
                        <small>Blacklisted at: ${new Date(device.blacklistedAt).toLocaleString()}</small>
                    </div>
                    <div class="action-buttons">
                        <button class="unblacklist-btn" data-mac="${device.mac}">Unblacklist</button>
                    </div>`;
                blacklistedList.appendChild(li);
            });
        } else {
            blacklistedList.innerHTML = '<li>No blacklisted devices.</li>'; //
        }
        addDeviceActionListeners(blacklistedList);
    }

    function updateConnectionLogsList(logs) {
        logList.innerHTML = ''; //
        if (logs && logs.length > 0) {
            logs.forEach(log => {
                const li = document.createElement('li');
                const logTime = new Date(log.timestamp).toLocaleString();
                li.innerHTML = `<div class="device-info"><small>${logTime} - [${log.status.toUpperCase()}] Device ${log.mac} (${log.deviceName || 'N/A'}) on AP ${log.ap}</small></div>`;
                logList.appendChild(li);
            });
        } else {
            logList.innerHTML = '<li>No connection logs available.</li>'; //
        }
    }

    // --- Action Listeners & API Calls ---
    function addDeviceActionListeners(ulElement) {
        ulElement.querySelectorAll('button').forEach(button => {
            button.onclick = (e) => {
                const mac = e.currentTarget.dataset.mac;
                const name = e.currentTarget.dataset.name;
                const action = e.currentTarget.className;

                if (action.includes('disconnect-btn')) disconnectDevice(mac, false); //
                else if (action.includes('blacklist-btn')) disconnectDevice(mac, true, name); //
                else if (action.includes('unblacklist-btn')) unblacklistDevice(mac); //
                else if (action.includes('notify-btn')) notifyDevice(mac); //
            };
        });
    }

    async function apiPost(endpoint, body) {
        try {
            const response = await fetch(endpoint, {
                method: 'POST', //
                headers: { 'Content-Type': 'application/json' }, //
                body: JSON.stringify(body) //
            });
            const result = await response.json();
            statusMessage.textContent = result.message; //
            statusMessage.style.color = response.ok ? 'green' : 'red'; //
            // After any action, refresh all lists immediately for responsive UI
            fetchAllLists();
        } catch (error) {
            statusMessage.textContent = 'Network error or server unreachable.'; //
            statusMessage.style.color = 'red'; //
        }
    }

    function disconnectDevice(mac, shouldBlacklist, deviceName = '') {
        const endpoint = shouldBlacklist ? '/api/disconnect-device' : '/api/disconnect-device-only'; //
        console.log(`Action: Disconnect for ${mac}, Blacklist: ${shouldBlacklist}, Name: ${deviceName}`);
        // Pass deviceName in the payload for blacklisting
        apiPost(endpoint, { mac, deviceName });
    }

    function unblacklistDevice(mac) {
        console.log(`Action: Unblacklist ${mac}`);
        apiPost('/api/unblacklist-device', { mac }); //
    }

    function notifyDevice(mac) {
        console.log(`Action: Notify ${mac}`);
        apiPost('/api/notify-device', { mac }); //
    }

    // --- Initial Setup & Polling ---
    showPage('devices'); //
    fetchAllLists(); // Initial fetch on page load
    setInterval(fetchAllLists, 3000); // Poll for new data every 3 seconds
});
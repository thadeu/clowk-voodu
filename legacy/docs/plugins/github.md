# Github Plugin

The Github plugin allows you to deploy your application from a GitHub repository.

## Installation

```bash
gokku plugins:add https://github.com/user/gokku-myplugin
```

## Usage

```bash
gokku services:create myplugin --name myplugin-service
```

## All commands inside the commands folder

```bash
gokku myplugin:help
gokku myplugin:info myplugin-service
gokku myplugin:logs myplugin-service
gokku myplugin:reload myplugin-service
gokku myplugin:status myplugin-service
```

## Strutructure of the commands folder

```
commands/
├── help
├── info
├── logs
├── reload
└── status
```


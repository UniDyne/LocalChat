export namespace main {
	
	export class ChatMessage {
	    role: string;
	    content: string;
	
	    static createFrom(source: any = {}) {
	        return new ChatMessage(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.role = source["role"];
	        this.content = source["content"];
	    }
	}

}

export namespace skill {
	
	export class Meta {
	    name: string;
	    description: string;
	    path: string;
	
	    static createFrom(source: any = {}) {
	        return new Meta(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.description = source["description"];
	        this.path = source["path"];
	    }
	}

}

export namespace store {
	
	export class Artifact {
	    id: string;
	    sessionId: string;
	    title: string;
	    content: string;
	    contentType: string;
	    createdAt: string;
	
	    static createFrom(source: any = {}) {
	        return new Artifact(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.sessionId = source["sessionId"];
	        this.title = source["title"];
	        this.content = source["content"];
	        this.contentType = source["contentType"];
	        this.createdAt = source["createdAt"];
	    }
	}
	export class ArtifactMeta {
	    id: string;
	    title: string;
	    contentType: string;
	    createdAt: string;
	
	    static createFrom(source: any = {}) {
	        return new ArtifactMeta(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.title = source["title"];
	        this.contentType = source["contentType"];
	        this.createdAt = source["createdAt"];
	    }
	}
	export class Session {
	    id: string;
	    title: string;
	    createdAt: string;
	    messageCount: number;
	
	    static createFrom(source: any = {}) {
	        return new Session(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.title = source["title"];
	        this.createdAt = source["createdAt"];
	        this.messageCount = source["messageCount"];
	    }
	}
	export class StoredMessage {
	    role: string;
	    content: string;
	    time: string;
	
	    static createFrom(source: any = {}) {
	        return new StoredMessage(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.role = source["role"];
	        this.content = source["content"];
	        this.time = source["time"];
	    }
	}

}

